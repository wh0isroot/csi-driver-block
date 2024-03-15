## CSI driver debug tips

### case#1: volume create/delete failed
> There could be multiple controller pods (only one pod is the leader), if there are no helpful logs, try to get logs from the leader controller pod.
 - find csi driver controller pod
```console
$ kubectl get pod -o wide -n kube-system | grep csi-block-controller
NAME                                     READY   STATUS    RESTARTS   AGE     IP             NODE
csi-block-controller-56bfddd689-dh5tk      5/5     Running   0          35s     10.240.0.19    k8s-agentpool-22533604-0
csi-block-controller-56bfddd689-sl4ll      5/5     Running   0          35s     10.240.0.23    k8s-agentpool-22533604-1
```
 - get pod description and logs
```console
$ kubectl describe csi-block-controller-56bfddd689-dh5tk -n kube-system > csi-block-controller-description.log
$ kubectl logs csi-block-controller-56bfddd689-dh5tk -c block -n kube-system > csi-block-controller.log
```

### case#2: volume mount/unmount failed
 - locate csi driver pod that does the actual volume mount/unmount

```console
$ kubectl get pod -o wide -n kube-system | grep csi-block-node
NAME                                      READY   STATUS    RESTARTS   AGE     IP             NODE
csi-block-node-cvgbs                        3/3     Running   0          7m4s    10.240.0.35    k8s-agentpool-22533604-1
csi-block-node-dr4s4                        3/3     Running   0          7m4s    10.240.0.4     k8s-agentpool-22533604-0
```

 - get pod description and logs
```console
$ kubectl describe po csi-block-node-cvgbs -n kube-system > csi-block-node-description.log
$ kubectl logs csi-block-node-cvgbs -c block -n kube-system > csi-block-node.log
```

 - check block mount inside driver
```console
kubectl exec -it csi-block-node-cvgbss -n kube-system -c block -- mount | grep block
```

### troubleshooting connection failure on agent node
```console
mkdir /tmp/test
mount -v -t block -o ... block-server:/path /tmp/test
```
