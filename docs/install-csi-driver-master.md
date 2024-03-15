# Install NFS CSI driver master version on a kubernetes cluster

If you have already installed Helm, you can also use it to install this driver. Please check [Installation with Helm](../charts/README.md).

## Install with kubectl
 - Option#1. remote install
```console
curl -skSL https://raw.githubusercontent.com/kubernetes-csi/csi-driver-block/master/deploy/install-driver.sh | bash -s master --
```

 - Option#2. local install
```console
git clone https://github.com/kubernetes-csi/csi-driver-block.git
cd csi-driver-block
./deploy/install-driver.sh master local
```

- check pods status:
```console
kubectl -n kube-system get pod -o wide -l app=csi-block-controller
kubectl -n kube-system get pod -o wide -l app=csi-block-node
```

example output:

```console
NAME                                       READY   STATUS    RESTARTS   AGE     IP             NODE
csi-block-controller-56bfddd689-dh5tk       4/4     Running   0          35s     10.240.0.19    k8s-agentpool-22533604-0
csi-block-node-cvgbs                        3/3     Running   0          35s     10.240.0.35    k8s-agentpool-22533604-1
csi-block-node-dr4s4                        3/3     Running   0          35s     10.240.0.4     k8s-agentpool-22533604-0
```

### clean up NFS CSI driver
 - Option#1. remote uninstall
```console
curl -skSL https://raw.githubusercontent.com/kubernetes-csi/csi-driver-block/master/deploy/uninstall-driver.sh | bash -s master --
```

 - Option#2. local uninstall
```console
git clone https://github.com/kubernetes-csi/csi-driver-block.git
cd csi-driver-block
git checkout master
./deploy/uninstall-driver.sh master local
```
