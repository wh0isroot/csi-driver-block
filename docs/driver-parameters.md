## Driver Parameters

> This driver requires existing and already configured blockv3 or blockv4 server, it supports dynamic provisioning of Persistent Volumes via Persistent Volume Claims by creating a new sub directory under block server.

### storage class usage (dynamic provisioning)

> [`StorageClass` example](../deploy/example/storageclass-block.yaml)

| Name             | Meaning                                                                                                     | Example Value                                                                      | Mandatory | Default value                                                       |
| ---------------- | ----------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------- | --------- | ------------------------------------------------------------------- |
| server           | block Server address                                                                                        | domain name `block-server.default.svc.cluster.local` <br>or IP address `127.0.0.1` | Yes       |
| share            | block share path                                                                                            | `/`                                                                                | Yes       |
| subDir           | sub directory under block share                                                                             |                                                                                    | No        | if sub directory does not exist, this driver would create a new one |
| mountPermissions | mounted folder permissions. The default is `0`, if set as non-zero, driver will perform `chmod` after mount |                                                                                    | No        |
| onDelete         | when volume is deleted, keep the directory if it's `retain`                                                 | `delete`(default), `retain`, `archive`                                             | No        | `delete`                                                            |

- VolumeID(`volumeHandle`) is the identifier of the volume handled by the driver, format of VolumeID:

```
{block-server-address}#{sub-dir-name}#{share-name}
```

> example: `block-server.default.svc.cluster.local/share#subdir#`

### PV/PVC usage (static provisioning)

> [`PersistentVolume` example](../deploy/example/pv-block-csi.yaml)

| Name                              | Meaning                                                                                                     | Example Value                                                                                                                                                                | Mandatory | Default value |
| --------------------------------- | ----------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------- | ------------- |
| volumeHandle                      | Specify a value the driver can use to uniquely identify the share in the cluster.                           | A recommended way to produce a unique value is to combine the block-server address, sub directory name and share name: `{block-server-address}#{sub-dir-name}#{share-name}`. | Yes       |
| volumeAttributes.server           | block Server address                                                                                        | domain name `block-server.default.svc.cluster.local` <br>or IP address `127.0.0.1`                                                                                           | Yes       |
| volumeAttributes.share            | block share path                                                                                            | `/`                                                                                                                                                                          | Yes       |
| volumeAttributes.mountPermissions | mounted folder permissions. The default is `0`, if set as non-zero, driver will perform `chmod` after mount |                                                                                                                                                                              | No        |

### Tips

#### `subDir` parameter supports following pv/pvc metadata conversion

> if `subDir` value contains following strings, it would be converted into corresponding pv/pvc name or namespace

- `${pvc.metadata.name}`
- `${pvc.metadata.namespace}`
- `${pv.metadata.name}`

#### provide `mountOptions` for `DeleteVolume`

> since `DeleteVolumeRequest` does not provide `mountOptions`, following is the workaround to provide `mountOptions` for `DeleteVolume`, check details [here](https://github.com/kubernetes-csi/csi-driver-block/issues/260)

- create a secret with `mountOptions`

```console
kubectl create secret generic mount-options --from-literal mountOptions="blockvers=3,hard"
```

- define a storage class with `csi.storage.k8s.io/provisioner-secret-name` and `csi.storage.k8s.io/provisioner-secret-namespace` setting:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: block-csi
provisioner: block.csi.k8s.io
parameters:
  server: block-server.default.svc.cluster.local
  share: /
  # csi.storage.k8s.io/provisioner-secret is only needed for providing mountOptions in DeleteVolume
  csi.storage.k8s.io/provisioner-secret-name: "mount-options"
  csi.storage.k8s.io/provisioner-secret-namespace: "default"
reclaimPolicy: Delete
volumeBindingMode: Immediate
mountOptions:
  - blockvers=4.1
```
