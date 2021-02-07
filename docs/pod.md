# Run in kubernetes as pod

> ⚠️ **Attention!** ⚠️
> 
> This is an example to demonstrate how it works.
> Don't do that in a production environment, don't store your token in kubernetes secret!

Create secret `kubeconfig` with kubernetes config in default namespace

```shell
kubectl create secret generic kubeconfig --from-file=$HOME/.kube/config
```

Apply pod in default namespace

```shell
kubectl apply -f deploy/pod.yaml
```

Execute dump in pod for all namespaces (if you can list namespaces)

```shell
kubectl exec -ti kube-dump -- /kube-dump dump-namespaces
```

or dump default namespace

```shell
kubectl exec -ti kube-dump -- /kube-dump -n default
```
