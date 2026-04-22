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
	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
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
	Name          string `json:"name"`
	ID            string `json:"id"`
	Size          int64  `json:"size"`
	Format        int    `json:"format"`
	Pool          string `json:"pool"`
	HasSnaps      bool
	HasClone      bool
	InTrash       bool
	State         string
	// RBD snapshot hierarchy fields
	ImageType     string   // "parent", "clone", "snapshot"
	ParentImage   string   // Parent image name (for clones)
	ParentSnap    string   // Parent snapshot name (for clones)
	ChildClones   []string // Child images created from this image's snapshots
	ActiveSnaps   []string // Active snapshots on this image (csi-snap-* format)
	HasDependents bool     // True if has children or active snapshots preventing deletion
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
	allImages = r.analyzeImageHierarchy(allImages)

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

	// Check for clones and extract parent information
	if parent, ok := imageInfo["parent"].(map[string]interface{}); ok && parent != nil {
		info.HasClone = true
		if parentImage, ok := parent["image"].(string); ok {
			info.ParentImage = parentImage
		}
		if parentSnap, ok := parent["snapshot"].(string); ok {
			info.ParentSnap = parentSnap
		}
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

// analyzeImageHierarchy analyzes the snapshot hierarchy for all images
func (r *RBDVolume) analyzeImageHierarchy(images []RBDImageInfo) []RBDImageInfo {
	// Create maps for quick lookups
	imageMap := make(map[string]*RBDImageInfo)
	for i := range images {
		imageMap[images[i].Name] = &images[i]
	}

	// First pass: identify image types and collect snapshot info
	for i := range images {
		img := &images[i]
		
		// Get snapshots for this image
		if !img.InTrash {
			snaps, err := r.listSnapshots(img.Name)
			if err == nil {
				for _, snap := range snaps {
					// Track csi-snap-* snapshots
					if strings.HasPrefix(snap.Name, "csi-snap-") {
						img.ActiveSnaps = append(img.ActiveSnaps, snap.Name)
					}
				}
			}
		}

		// Determine image type
		if img.HasClone {
			// This is a clone (has a parent)
			if strings.HasPrefix(img.Name, "csi-snap-") {
				img.ImageType = "clone-snapshot"
			} else {
				img.ImageType = "clone"
			}
		} else if strings.HasPrefix(img.Name, "csi-snap-") {
			img.ImageType = "snapshot-clone"
		} else if strings.HasPrefix(img.Name, "csi-vol-") {
			img.ImageType = "parent"
		} else {
			img.ImageType = "other"
		}
	}

	// Second pass: build parent-child relationships
	for i := range images {
		img := &images[i]
		
		// Find children: images that have this image as parent
		for j := range images {
			child := &images[j]
			if child.ParentImage == img.Name {
				img.ChildClones = append(img.ChildClones, child.Name)
			}
		}

		// Determine if image has dependents
		img.HasDependents = len(img.ChildClones) > 0 || len(img.ActiveSnaps) > 0
	}

	return images
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

// getActiveVolumeSnapshotContentImages returns a map of image names that have corresponding VolumeSnapshotContent
func (r *RBDVolume) getActiveVolumeSnapshotContentImages() map[string]bool {
	vscList, err := r.Clientsets.Dynamic.Resource(snapshotv1.SchemeGroupVersion.WithResource("volumesnapshotcontents")).List(r.Ctx, metav1.ListOptions{})
	if err != nil {
		logging.Warning("failed to list VolumeSnapshotContents: %v", err)
		return nil
	}

	activeSnapImages := make(map[string]bool)
	for _, vsc := range vscList.Items {
		// Extract volumeHandle from VolumeSnapshotContent
		if snapshotHandle, found, err := getNestedString(vsc.Object, "status", "snapshotHandle"); found && err == nil {
			// snapshotHandle format: "0001-0011-openshift-storage-0000000000000001-6a15ae78-bf7f-4b41-b2c4-5cb32a3e80a2"
			// Extract the UUID part (last segment after the last dash)
			parts := strings.Split(snapshotHandle, "-")
			if len(parts) >= 5 {
				// The UUID is the last 5 parts: 6a15ae78-bf7f-4b41-b2c4-5cb32a3e80a2
				uuid := strings.Join(parts[len(parts)-5:], "-")
				imageName := "csi-snap-" + uuid
				activeSnapImages[imageName] = true
			}
		}
	}

	return activeSnapImages
}

// getNestedString retrieves a nested string value from an unstructured object
func getNestedString(obj map[string]interface{}, fields ...string) (string, bool, error) {
	val, found, err := getNestedField(obj, fields...)
	if !found || err != nil {
		return "", found, err
	}
	s, ok := val.(string)
	if !ok {
		return "", false, fmt.Errorf("value is not a string")
	}
	return s, true, nil
}

// getNestedField retrieves a nested field from an unstructured object
func getNestedField(obj map[string]interface{}, fields ...string) (interface{}, bool, error) {
	var val interface{} = obj
	for _, field := range fields {
		m, ok := val.(map[string]interface{})
		if !ok {
			return nil, false, fmt.Errorf("value is not a map")
		}
		val, ok = m[field]
		if !ok {
			return nil, false, nil
		}
	}
	return val, true, nil
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
	activeSnapImages := r.getActiveVolumeSnapshotContentImages()
	
	if activePVImages == nil {
		activePVImages = make(map[string]bool)
	}
	if activeSnapImages == nil {
		activeSnapImages = make(map[string]bool)
	}

	for i := range images {
		if activePVImages[images[i].Name] {
			images[i].State = "in-use"
		} else if activeSnapImages[images[i].Name] {
			images[i].State = "in-use"
		} else if strings.HasPrefix(images[i].Name, "csi-vol-") {
			if images[i].HasSnaps {
				images[i].State = "stale-with-snapshot"
			} else {
				images[i].State = "stale"
			}
		} else if strings.HasPrefix(images[i].Name, "csi-snap-") {
			if images[i].HasSnaps {
				images[i].State = "stale-with-snapshot"
			} else {
				images[i].State = "stale"
			}
		}
	}

	return images
}

// printImages prints RBD image information with hierarchy details
func (r *RBDVolume) printImages(images []RBDImageInfo) {
	if len(images) == 0 {
		logging.Info("No RBD images found")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tTYPE\tPARENT\tCHILDREN\tSNAPSHOTS\tDEPENDENTS\tTRASH\tSTATE")

	for _, img := range images {
		// Format image type
		imageType := img.ImageType
		if imageType == "" {
			imageType = "-"
		}

		// Format parent info
		parent := "-"
		if img.ParentImage != "" {
			if img.ParentSnap != "" {
				parent = fmt.Sprintf("%s@%s", img.ParentImage, img.ParentSnap)
			} else {
				parent = img.ParentImage
			}
		}

		// Format children count
		children := "-"
		if len(img.ChildClones) > 0 {
			children = fmt.Sprintf("%d", len(img.ChildClones))
		}

		// Format snapshots count
		snapshots := "-"
		if len(img.ActiveSnaps) > 0 {
			snapshots = fmt.Sprintf("%d", len(img.ActiveSnaps))
		} else if img.HasSnaps {
			snapshots = "Yes"
		}

		// Format dependents status
		dependents := "No"
		if img.HasDependents {
			dependents = "Yes"
		}

		// Format trash status
		trash := "No"
		if img.InTrash {
			trash = "Yes"
		}

		// Format state
		state := img.State
		if state == "" {
			state = "-"
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			img.Name, imageType, parent, children, snapshots, dependents, trash, state)
	}
	w.Flush()
}

// Delete deletes an RBD image or trash entry
func (r *RBDVolume) Delete(imageName string) {
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

	// Check for dependencies
	if info.HasSnaps {
		logging.Fatal(fmt.Errorf("image has snapshots. Delete snapshots first using 'odf rbdvolume delete-snapshot'"))
	}

	if info.HasClone {
		logging.Fatal(fmt.Errorf("image is a clone. Flatten the image first using 'odf rbdvolume flatten'"))
	}

	// Check for child clones
	images, err := r.listRBDImages()
	if err == nil {
		allImages := r.analyzeImageHierarchy(images)
		for _, img := range allImages {
			if img.Name == imageName && len(img.ChildClones) > 0 {
				logging.Fatal(fmt.Errorf("image has %d child clone(s). Delete child clones first: %v", len(img.ChildClones), img.ChildClones))
			}
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

// Flatten flattens a cloned RBD image to remove parent dependency
func (r *RBDVolume) Flatten(imageName string) {
	logging.Info("Flattening image: %s/%s", r.PoolName, imageName)
	
	info, err := r.getImageInfo(imageName)
	if err != nil {
		logging.Fatal(fmt.Errorf("failed to get image info: %v", err))
	}

	if !info.HasClone {
		logging.Fatal(fmt.Errorf("image %s is not a clone and does not need flattening", imageName))
	}

	args := []string{"flatten", fmt.Sprintf("%s/%s", r.PoolName, imageName)}
	_, err = exec.RunCommandInOperatorPod(r.Ctx, r.Clientsets, "rbd", args, r.OperatorNamespace, r.ClusterNamespace, false)
	if err != nil {
		logging.Fatal(fmt.Errorf("failed to flatten image: %v", err))
	}

	logging.Info("Successfully flattened image: %s", imageName)
}

// DeleteSnapshot deletes a specific snapshot from an RBD image
func (r *RBDVolume) DeleteSnapshot(imageName, snapshotName string) {
	logging.Info("Deleting snapshot: %s@%s", imageName, snapshotName)

	// Verify the snapshot exists
	snaps, err := r.listSnapshots(imageName)
	if err != nil {
		logging.Fatal(fmt.Errorf("failed to list snapshots: %v", err))
	}

	found := false
	for _, snap := range snaps {
		if snap.Name == snapshotName {
			found = true
			break
		}
	}

	if !found {
		logging.Fatal(fmt.Errorf("snapshot %s not found on image %s", snapshotName, imageName))
	}

	// Check if snapshot has child clones
	images, err := r.listRBDImages()
	if err == nil {
		for _, img := range images {
			if img.ParentImage == imageName && img.ParentSnap == snapshotName {
				logging.Fatal(fmt.Errorf("snapshot %s@%s has child clone: %s. Delete or flatten the child clone first", imageName, snapshotName, img.Name))
			}
		}
	}

	args := []string{"snap", "rm", fmt.Sprintf("%s/%s@%s", r.PoolName, imageName, snapshotName)}
	_, err = exec.RunCommandInOperatorPod(r.Ctx, r.Clientsets, "rbd", args, r.OperatorNamespace, r.ClusterNamespace, false)
	if err != nil {
		logging.Fatal(fmt.Errorf("failed to delete snapshot: %v", err))
	}

	logging.Info("Successfully deleted snapshot: %s@%s", imageName, snapshotName)
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
