package rbd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/rook/kubectl-rook-ceph/pkg/exec"
	"github.com/rook/kubectl-rook-ceph/pkg/k8sutil"
	"github.com/rook/kubectl-rook-ceph/pkg/logging"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type RBDVolume struct {
	Ctx               context.Context
	Clientsets        *k8sutil.Clientsets
	OperatorNamespace string
	ClusterNamespace  string
	PoolName          string
}

type RBDImageInfo struct {
	Name     string `json:"name"`
	ID       string `json:"id"`
	Size     int64  `json:"size"`
	Format   int    `json:"format"`
	Pool     string `json:"pool"`
	HasSnaps bool
	HasClone bool
	InTrash  bool
	State    string
}

type RBDSnapInfo struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// List lists RBD images in the pool, optionally filtering for stale volumes
func (r *RBDVolume) List(staleOnly bool) {
	images, err := r.listRBDImages()
	if err != nil {
		logging.Fatal(fmt.Errorf("failed to list RBD images: %v", err))
	}

	trashImages, err := r.listRBDTrash()
	if err != nil {
		logging.Warning("failed to list RBD trash: %v", err)
		trashImages = []RBDImageInfo{}
	}

	allImages := append(images, trashImages...)
	allImages = r.markImagesInUse(allImages)

	if staleOnly {
		staleImages := r.filterStaleImages(allImages)
		r.printImages(staleImages)
	} else {
		r.printImages(allImages)
	}
}

// listRBDImages lists all RBD images in the pool
func (r *RBDVolume) listRBDImages() ([]RBDImageInfo, error) {
	args := []string{"ls", "-p", r.PoolName, "--format", "json"}
	output, err := exec.RunCommandInOperatorPod(r.Ctx, r.Clientsets, "rbd", args, r.OperatorNamespace, r.ClusterNamespace, true)
	if err != nil {
		return nil, err
	}

	var imageNames []string
	if err := json.Unmarshal([]byte(output), &imageNames); err != nil {
		return nil, fmt.Errorf("failed to parse RBD image list: %v", err)
	}

	var images []RBDImageInfo
	for _, name := range imageNames {
		info, err := r.getImageInfo(name)
		if err != nil {
			logging.Warning("failed to get info for image %s: %v", name, err)
			continue
		}
		images = append(images, info)
	}

	return images, nil
}

// listRBDTrash lists RBD images in trash
func (r *RBDVolume) listRBDTrash() ([]RBDImageInfo, error) {
	args := []string{"trash", "ls", "-p", r.PoolName, "--format", "json"}
	output, err := exec.RunCommandInOperatorPod(r.Ctx, r.Clientsets, "rbd", args, r.OperatorNamespace, r.ClusterNamespace, true)
	if err != nil {
		return nil, err
	}

	var trashItems []map[string]interface{}
	if err := json.Unmarshal([]byte(output), &trashItems); err != nil {
		return nil, fmt.Errorf("failed to parse RBD trash list: %v", err)
	}

	var images []RBDImageInfo
	for _, item := range trashItems {
		name, _ := item["name"].(string)
		id, _ := item["id"].(string)

		// Get detailed info using image ID
		info, err := r.getImageInfoByID(id)
		if err != nil {
			logging.Warning("failed to get info for trash image %s (ID: %s): %v", name, id, err)
			// Add basic info even if detailed fetch fails
			images = append(images, RBDImageInfo{
				Name:    name,
				ID:      id,
				Pool:    r.PoolName,
				InTrash: true,
			})
			continue
		}
		info.InTrash = true
		images = append(images, info)
	}

	return images, nil
}

// getImageInfo retrieves detailed information about an RBD image
func (r *RBDVolume) getImageInfo(imageName string) (RBDImageInfo, error) {
	return r.getImageInfoByName(imageName, false, "")
}

// getImageInfoByID retrieves detailed information about an RBD image using its ID (for trash)
func (r *RBDVolume) getImageInfoByID(imageID string) (RBDImageInfo, error) {
	return r.getImageInfoByName("", true, imageID)
}

// getImageInfoByName retrieves detailed information about an RBD image
func (r *RBDVolume) getImageInfoByName(imageName string, useID bool, imageID string) (RBDImageInfo, error) {
	info := RBDImageInfo{
		Name: imageName,
		ID:   imageID,
		Pool: r.PoolName,
	}

	// Get image info
	var args []string
	if useID {
		args = []string{"info", "-p", r.PoolName, "--image-id", imageID, "--format", "json"}
	} else {
		args = []string{"info", fmt.Sprintf("%s/%s", r.PoolName, imageName), "--format", "json"}
	}

	output, err := exec.RunCommandInOperatorPod(r.Ctx, r.Clientsets, "rbd", args, r.OperatorNamespace, r.ClusterNamespace, true)
	if err != nil {
		return info, err
	}

	var imageInfo map[string]interface{}
	if err := json.Unmarshal([]byte(output), &imageInfo); err != nil {
		return info, err
	}

	if size, ok := imageInfo["size"].(float64); ok {
		info.Size = int64(size)
	}
	if format, ok := imageInfo["format"].(float64); ok {
		info.Format = int(format)
	}
	if name, ok := imageInfo["name"].(string); ok && info.Name == "" {
		info.Name = name
	}
	if id, ok := imageInfo["id"].(string); ok && info.ID == "" {
		info.ID = id
	}

	// Check for snapshots (only for non-trash images)
	if !useID {
		snaps, err := r.listSnapshots(imageName)
		if err == nil && len(snaps) > 0 {
			info.HasSnaps = true
		}
	}

	// Check for clones
	if parent, ok := imageInfo["parent"].(map[string]interface{}); ok && parent != nil {
		info.HasClone = true
	}

	return info, nil
}

// listSnapshots lists snapshots for an RBD image
func (r *RBDVolume) listSnapshots(imageName string) ([]RBDSnapInfo, error) {
	args := []string{"snap", "ls", fmt.Sprintf("%s/%s", r.PoolName, imageName), "--format", "json"}
	output, err := exec.RunCommandInOperatorPod(r.Ctx, r.Clientsets, "rbd", args, r.OperatorNamespace, r.ClusterNamespace, true)
	if err != nil {
		return nil, err
	}

	var snaps []RBDSnapInfo
	if err := json.Unmarshal([]byte(output), &snaps); err != nil {
		return nil, err
	}

	return snaps, nil
}

func (r *RBDVolume) getActivePVImages() map[string]bool {
	pvList, err := r.Clientsets.Kube.CoreV1().PersistentVolumes().List(r.Ctx, metav1.ListOptions{})
	if err != nil {
		logging.Warning("failed to list PVs: %v", err)
		return nil
	}

	activePVImages := make(map[string]bool)
	for _, pv := range pvList.Items {
		if pv.Spec.CSI != nil && pv.Spec.CSI.Driver == "openshift-storage.rbd.csi.ceph.com" {
			if imageName, ok := pv.Spec.CSI.VolumeAttributes["imageName"]; ok {
				activePVImages[imageName] = true
			}
		}
	}

	return activePVImages
}

// filterStaleImages filters images that don't have corresponding PVs
func (r *RBDVolume) filterStaleImages(images []RBDImageInfo) []RBDImageInfo {
	activePVImages := r.getActivePVImages()
	if activePVImages == nil {
		return images
	}

	var staleImages []RBDImageInfo
	for _, img := range images {
		if strings.HasPrefix(img.Name, "csi-vol-") && !activePVImages[img.Name] {
			staleImages = append(staleImages, img)
		}
	}

	return staleImages
}

func (r *RBDVolume) markImagesInUse(images []RBDImageInfo) []RBDImageInfo {
	activePVImages := r.getActivePVImages()
	if activePVImages == nil {
		return images
	}

	for i := range images {
		if activePVImages[images[i].Name] {
			images[i].State = "in-use"
		} else if strings.HasPrefix(images[i].Name, "csi-vol-") {
			if images[i].HasSnaps {
				images[i].State = "stale-with-snapshot"
			} else {
				images[i].State = "stale"
			}
		}
	}

	return images
}

// printImages prints RBD image information
func (r *RBDVolume) printImages(images []RBDImageInfo) {
	if len(images) == 0 {
		logging.Info("No RBD images found")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tPOOL\tSNAPS\tCLONE\tTRASH\tSTATE")

	for _, img := range images {
		snaps := "No"
		if img.HasSnaps {
			snaps = "Yes"
		}
		clone := "No"
		if img.HasClone {
			clone = "Yes"
		}
		trash := "No"
		if img.InTrash {
			trash = "Yes"
		}
		state := img.State
		if state == "" {
			state = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", img.Name, img.Pool, snaps, clone, trash, state)
	}
	w.Flush()
}

// Delete deletes an RBD image, handling snapshots and clones
func (r *RBDVolume) Delete(imageName string, force bool) {
	logging.Info("Deleting RBD image: %s", imageName)

	trashImages, err := r.listRBDTrash()
	if err != nil {
		logging.Warning("failed to list RBD trash while checking %s: %v", imageName, err)
	} else {
		for _, img := range trashImages {
			if img.ID == imageName || img.Name == imageName {
				r.deleteFromTrash(img.ID)
				return
			}
		}
	}

	info, err := r.getImageInfo(imageName)
	if err != nil {
		logging.Fatal(fmt.Errorf("failed to get image info: %v", err))
	}

	// Handle snapshots
	if info.HasSnaps {
		if !force {
			logging.Fatal(fmt.Errorf("image has snapshots. Use --force to delete snapshots first"))
		}
		if err := r.deleteSnapshots(imageName); err != nil {
			logging.Fatal(fmt.Errorf("failed to delete snapshots: %v", err))
		}
	}

	// Handle clones
	if info.HasClone {
		if !force {
			logging.Fatal(fmt.Errorf("image is a clone. Use --force to flatten first"))
		}
		if err := r.flattenImage(imageName); err != nil {
			logging.Fatal(fmt.Errorf("failed to flatten image: %v", err))
		}
	}

	// Delete the image
	args := []string{"rm", fmt.Sprintf("%s/%s", r.PoolName, imageName)}
	_, err = exec.RunCommandInOperatorPod(r.Ctx, r.Clientsets, "rbd", args, r.OperatorNamespace, r.ClusterNamespace, false)
	if err != nil {
		logging.Fatal(fmt.Errorf("failed to delete image: %v", err))
	}

	logging.Info("Successfully deleted RBD image: %s", imageName)
}

// deleteSnapshots deletes all snapshots of an RBD image
func (r *RBDVolume) deleteSnapshots(imageName string) error {
	snaps, err := r.listSnapshots(imageName)
	if err != nil {
		return err
	}

	for _, snap := range snaps {
		logging.Info("Deleting snapshot: %s@%s", imageName, snap.Name)
		args := []string{"snap", "rm", fmt.Sprintf("%s/%s@%s", r.PoolName, imageName, snap.Name)}
		_, err := exec.RunCommandInOperatorPod(r.Ctx, r.Clientsets, "rbd", args, r.OperatorNamespace, r.ClusterNamespace, false)
		if err != nil {
			return fmt.Errorf("failed to delete snapshot %s: %v", snap.Name, err)
		}
	}

	return nil
}

// flattenImage flattens a cloned RBD image
func (r *RBDVolume) flattenImage(imageName string) error {
	logging.Info("Flattening image: %s/%s", r.PoolName, imageName)
	args := []string{"flatten", fmt.Sprintf("%s/%s", r.PoolName, imageName)}
	_, err := exec.RunCommandInOperatorPod(r.Ctx, r.Clientsets, "rbd", args, r.OperatorNamespace, r.ClusterNamespace, false)
	return err
}

// deleteFromTrash removes an image from RBD trash
func (r *RBDVolume) deleteFromTrash(imageID string) {
	logging.Info("Removing image from trash: %s", imageID)
	args := []string{"trash", "rm", "-p", r.PoolName, imageID}
	_, err := exec.RunCommandInOperatorPod(r.Ctx, r.Clientsets, "rbd", args, r.OperatorNamespace, r.ClusterNamespace, false)
	if err != nil {
		logging.Fatal(fmt.Errorf("failed to remove image from trash: %v", err))
	}
	logging.Info("Successfully removed image from trash")
}

// GetPVsUsingRBD returns list of PVs using RBD CSI driver
func (r *RBDVolume) GetPVsUsingRBD() ([]corev1.PersistentVolume, error) {
	pvList, err := r.Clientsets.Kube.CoreV1().PersistentVolumes().List(r.Ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	var rbdPVs []corev1.PersistentVolume
	for _, pv := range pvList.Items {
		if pv.Spec.CSI != nil && pv.Spec.CSI.Driver == "openshift-storage.rbd.csi.ceph.com" {
			rbdPVs = append(rbdPVs, pv)
		}
	}

	return rbdPVs, nil
}

// Made with Bob
