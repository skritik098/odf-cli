# RBD Volume Management

The `odf rbdvolume` command provides tools to manage RBD (RADOS Block Device) volumes in your ODF cluster, particularly for cleaning up orphaned or stale volumes that were created with Retain storage class policy.

## Commands

### List RBD Volumes

List all RBD volumes in the specified pool:

```bash
odf rbdvolume ls
```

List only stale/orphaned RBD volumes (volumes without corresponding PersistentVolumes):

```bash
odf rbdvolume ls --stale
```

Use a custom pool name:

```bash
odf rbdvolume ls --pool my-custom-pool
```

### Delete RBD Volume

Delete a specific RBD volume or trash entry:

```bash
odf rbdvolume delete <image-name-or-id>
```

**Note**: The delete command will fail if the image has dependencies (snapshots, child clones, or is a clone itself). You must resolve these dependencies first using the appropriate commands below.

Use a custom pool:

```bash
odf rbdvolume delete <image-name-or-id> --pool my-custom-pool
```

### Flatten RBD Image

Flatten a cloned RBD image to remove its parent dependency:

```bash
odf rbdvolume flatten <image-name>
```

This command removes the parent-child relationship, making the clone independent. Use this before deleting a clone image.

Use a custom pool:

```bash
odf rbdvolume flatten <image-name> --pool my-custom-pool
```

### Delete Snapshot

Delete a specific snapshot from an RBD image:

```bash
odf rbdvolume delete-snapshot <image-name> <snapshot-name>
```

**Note**: The command will fail if the snapshot has child clones. Delete or flatten the child clones first.

Use a custom pool:

```bash
odf rbdvolume delete-snapshot <image-name> <snapshot-name> --pool my-custom-pool
```

## Flags

### Global Flags

- `--pool`: RBD pool name (default: `ocs-storagecluster-cephblockpool`)

### List Command Flags

- `--stale`: Filter to show only stale volumes (volumes without corresponding PVs or VolumeSnapshotContents)

## Cleanup Process for Stale Volumes

### Understanding the Hierarchy

Before cleaning up stale volumes, it's crucial to understand the RBD snapshot hierarchy. Use the `ls` command to identify the dependency chain:

```bash
odf rbdvolume ls
```

Look at the output columns:
- **PARENT**: Shows which image this clone depends on
- **CHILDREN**: Number of child clones
- **SNAPSHOTS**: Number of active snapshots
- **DEPENDENTS**: Whether the image has dependencies preventing deletion

### Identifying Leaf Nodes

A **leaf node** is an image that has no dependents (DEPENDENTS column shows "No"). These are safe to delete first. Leaf nodes typically are:
- Images with no child clones
- Images with no snapshots
- The last image in a dependency chain

### Step-by-Step Cleanup Process

**IMPORTANT**: Always delete in reverse order of the dependency chain, starting from leaf nodes.

#### Example Scenario

Let's say `odf rbdvolume ls` shows this hierarchy:

```
NAME                   TYPE              PARENT                          CHILDREN  SNAPSHOTS  DEPENDENTS  TRASH  STATE
csi-vol-abc123         parent            -                               1         1          Yes         No     stale
csi-snap-xyz789        clone-snapshot    csi-vol-abc123@temp-snap        1         1          Yes         No     stale
csi-snap-restore-001   clone             csi-snap-xyz789@csi-snap-xyz    0         0          No          No     stale
```

#### Cleanup Steps (Reverse Order):

1. **Identify the leaf node** (image with DEPENDENTS = No):
   ```bash
   # In this example, csi-snap-restore-001 is the leaf node
   odf rbdvolume ls
   ```

2. **Delete the leaf node first**:
   ```bash
   odf rbdvolume delete csi-snap-restore-001
   ```

3. **Delete snapshots on the next level**:
   ```bash
   # List snapshots on csi-snap-xyz789
   odf rbdvolume delete-snapshot csi-snap-xyz789 csi-snap-xyz
   ```

4. **Delete the clone image**:
   ```bash
   odf rbdvolume delete csi-snap-xyz789
   ```

5. **Delete snapshots on the parent**:
   ```bash
   # Delete the snapshot on the parent volume
   odf rbdvolume delete-snapshot csi-vol-abc123 temp-snap
   ```

6. **Delete the parent volume**:
   ```bash
   odf rbdvolume delete csi-vol-abc123
   ```

### Quick Cleanup Workflow

1. **List all stale volumes**:
   ```bash
   odf rbdvolume ls --stale
   ```

2. **Identify leaf nodes** (DEPENDENTS = No):
   - Look for images with no children and no snapshots
   - These are safe to delete first

3. **Delete leaf nodes**:
   ```bash
   odf rbdvolume delete <leaf-node-image-name>
   ```

4. **For clones that need flattening**:
   ```bash
   odf rbdvolume flatten <clone-image-name>
   odf rbdvolume delete <clone-image-name>
   ```

5. **Delete snapshots before deleting parent images**:
   ```bash
   odf rbdvolume delete-snapshot <image-name> <snapshot-name>
   ```

6. **Repeat** until all stale volumes are cleaned up

### Handling Trash Entries

For volumes in trash, use the delete command with the image ID:
```bash
odf rbdvolume delete <image-id-from-trash>
```

## Understanding Volume States

RBD volumes can have the following states:

- **in-use**: Volume has a corresponding PersistentVolume or VolumeSnapshotContent in the cluster and is actively being used
- **stale**: CSI-created volume (name starts with `csi-vol-*` or `csi-snap-*`) without a corresponding PersistentVolume or VolumeSnapshotContent, and has no snapshots
- **stale-with-snapshot**: CSI-created volume without a corresponding PersistentVolume or VolumeSnapshotContent, but has snapshots that need to be handled first

Stale volumes were typically created with a Retain reclaim policy and not properly cleaned up. These volumes can accumulate over time and consume storage space unnecessarily.

### State Detection for csi-snap-* Images

For images with the `csi-snap-*` format, the state is determined by checking VolumeSnapshotContent resources. The volumeHandle in VolumeSnapshotContent contains information like:
```
volumeHandle: 0001-0011-openshift-storage-0000000000000001-6a15ae78-bf7f-4b41-b2c4-5cb32a3e80a2
```

The UUID portion (`6a15ae78-bf7f-4b41-b2c4-5cb32a3e80a2`) is used to construct the image name (`csi-snap-6a15ae78-bf7f-4b41-b2c4-5cb32a3e80a2`), which is then matched against existing VolumeSnapshotContent resources to determine if the image is in-use or stale.

## Understanding RBD Snapshot Hierarchy

RBD uses a complex snapshot and clone hierarchy that differs from CephFS:

### Snapshot Creation Flow
1. **Parent RBD Image** (`csi-vol-*`) - Original volume
2. → **Temporary Snapshot** (random name) - Created first, then moved to trash
3. → **Child Clone** (`csi-snap-*`) - Clone created from the temporary snapshot
4. → **Snapshot on Child** (`csi-snap-*`) - Snapshot created on the child clone with same name format
5. → **Restore PV** - Another child clone created from the child's snapshot when restoring

### Key Differences from CephFS
- **CephFS**: Snapshot → Clone (independent volume) → Can delete snapshot and parent directly
- **RBD**: Snapshot → Child Clone → Snapshot on Child → Restore Clone (complex dependency chain)

### Output Columns Explained

The `rbdvolume ls` command displays the following columns:

- **NAME**: Image name (e.g., `csi-vol-*`, `csi-snap-*`)
- **TYPE**: Image type in the hierarchy:
  - `parent`: Original parent volume
  - `clone`: Clone image created from a snapshot
  - `clone-snapshot`: Clone that also has a parent (csi-snap-* format)
  - `snapshot-clone`: Snapshot-based clone
  - `other`: Other image types
- **PARENT**: Parent image and snapshot this clone depends on (format: `parent-image@snapshot-name`)
- **CHILDREN**: Number of child clones created from this image's snapshots
- **SNAPSHOTS**: Number of active snapshots on this image (csi-snap-* format)
- **DEPENDENTS**: Whether this image has dependents (children or snapshots) that prevent safe deletion
- **TRASH**: Whether the image is in RBD trash
- **STATE**: Usage state (in-use, stale, stale-with-snapshot)

## Handling Snapshots and Clones

RBD volumes may have complex dependencies due to the snapshot hierarchy:

- **Snapshots**: Point-in-time copies of the volume
- **Clones**: Volumes created from snapshots (parent-child relationship)
- **Child Clones**: Additional clones created from snapshots on clone images
- **Dependents**: Any child clones or active snapshots that depend on this image

### Deletion Considerations

**IMPORTANT**: Due to RBD's snapshot hierarchy, you must delete in the correct order:

1. **Delete child clones first** (restore PVs created from snapshots)
2. **Delete snapshots** on clone images (csi-snap-* snapshots)
3. **Delete clone images** (csi-snap-* images)
4. **Delete parent volume** (original csi-vol-* image)

The `DEPENDENTS` column shows "Yes" if an image has children or snapshots that must be deleted first.

### Safe Deletion Guidelines

The delete command enforces safe deletion by checking for dependencies:

1. **Images with snapshots**: You must delete snapshots first using `delete-snapshot`
2. **Clone images**: You must flatten the image first using `flatten`
3. **Images with child clones**: You must delete child clones first

This prevents accidental data loss and ensures proper cleanup of the dependency chain.

## Important Notes

- **Always verify volumes are truly orphaned before deletion** - Check the STATE column
- **Delete in reverse order** - Start from leaf nodes (DEPENDENTS = No) and work backwards
- **Use `--stale` flag** to filter out active volumes and focus on cleanup candidates
- **Check dependencies** - The DEPENDENTS column shows if an image has children or snapshots
- **Understand the hierarchy** - Use the PARENT and CHILDREN columns to map the dependency chain
- The default pool name is `ocs-storagecluster-cephblockpool`
- Trash entries can be deleted with the same `delete` command by passing the image ID
- The command uses the RBD CSI driver identifier: `openshift-storage.rbd.csi.ceph.com`
- For `csi-snap-*` images, state is determined by checking VolumeSnapshotContent resources

## Troubleshooting

### "Image has snapshots" Error
Delete snapshots first:
```bash
odf rbdvolume delete-snapshot <image-name> <snapshot-name>
```

### "Image is a clone" Error
Flatten the image first:
```bash
odf rbdvolume flatten <image-name>
```

### "Image has child clones" Error
Delete child clones first (work from leaf nodes backwards):
```bash
odf rbdvolume delete <child-clone-name>
```

### "Snapshot has child clone" Error
Delete or flatten the child clone first:
```bash
odf rbdvolume flatten <child-clone-name>
odf rbdvolume delete <child-clone-name>
```