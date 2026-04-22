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

Force delete (automatically removes snapshots and flattens clones for regular images):

```bash
odf rbdvolume delete <image-name> --force
```

Use a custom pool:

```bash
odf rbdvolume delete <image-name-or-id> --pool my-custom-pool
```

## Flags

### Global Flags

- `--pool`: RBD pool name (default: `ocs-storagecluster-cephblockpool`)
- `--stale`: Filter to show only stale volumes (volumes without corresponding PVs)

### Delete Command Flags

- `--force`: Force deletion by automatically removing snapshots and flattening clones

## Examples

### Cleanup Workflow

1. List all stale RBD volumes:
   ```bash
   odf rbdvolume ls --stale
   ```

2. Review the output to identify orphaned volumes (typically named `csi-vol-*`) and check their STATE.

3. Delete a stale volume (with force flag if it has snapshots or is a clone):
   ```bash
   odf rbdvolume delete csi-vol-12345678-1234-1234-1234-123456789012 --force
   ```

4. For volumes in trash, use the same delete command with the image ID:
   ```bash
   odf rbdvolume delete <image-id-from-trash>
   ```

## Understanding Volume States

RBD volumes can have the following states:

- **in-use**: Volume has a corresponding PersistentVolume in the cluster and is actively being used
- **stale**: CSI-created volume (name starts with `csi-vol-`) without a corresponding PersistentVolume, and has no snapshots
- **stale-with-snapshot**: CSI-created volume without a corresponding PersistentVolume, but has snapshots that need to be handled

Stale volumes were typically created with a Retain reclaim policy and not properly cleaned up. These volumes can accumulate over time and consume storage space unnecessarily.

## Handling Snapshots and Clones

RBD volumes may have dependencies:

- **Snapshots**: Point-in-time copies of the volume
- **Clones**: Volumes created from snapshots (parent-child relationship)

When deleting volumes with dependencies:
1. Without `--force`: The command will fail and inform you about the dependencies
2. With `--force`: The command will:
   - Delete all snapshots first
   - Flatten clones (remove parent dependency)
   - Then delete the volume

## Notes

- Always verify volumes are truly orphaned before deletion
- Use `--stale` flag to filter out active volumes
- The `ls` output includes a `STATE` column showing the volume state: `in-use`, `stale`, or `stale-with-snapshot`
- The default pool name is `ocs-storagecluster-cephblockpool`
- Trash entries can be deleted with the same `delete` command by passing the image ID
- The command uses the RBD CSI driver identifier: `openshift-storage.rbd.csi.ceph.com`