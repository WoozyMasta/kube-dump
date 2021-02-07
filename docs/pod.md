# Run in kubernetes as pod

## Deploy with kubectl config

> ⚠️ **Attention!** ⚠️
> 
> This is an example to demonstrate how it works.  
> Don't do that in a production environment,
> don't store your token in kubernetes secret!

Create secret `kubeconfig` with kubernetes config in default namespace

```shell
kubectl create secret generic kubeconfig --from-file=$HOME/.kube/config
```

Apply pod in default namespace

```shell
kubectl apply -f deploy/pod-kubeconfig.yaml
```

Execute dump in pod for all namespaces (if you can list namespaces)

```shell
kubectl exec -ti kube-dump -- /kube-dump dump-namespaces
```

or dump default namespace

```shell
kubectl exec -ti kube-dump -- /kube-dump -n default
```

## Deploy with serviceaccount and volume

> ⚠️ **Attention!** ⚠️
> 
> This is an example to demonstrate how it works.  
> Do not do this in a production environment,
> the example below uses the cluster view role `cluster-role-view.yaml`,
> you have enough view permissions to dump the data.
> 
> Configure your role correctly according to your needs.
> You can find configuration examples in the `/deploy` directory.

Apply cluster admin sericeaccount, pvc, pod in kube-dump namespace:

```shell
kubectl create ns kube-dump
kubectl apply -n kube-dump -f deploy/cluster-role-view.yaml
kubectl apply -n kube-dump -f deploy/pvc.yaml
kubectl apply -n kube-dump -f deploy/pod-kubeconfig.yaml
```

## Deploy with serviceaccount, ssh key and volume

```shell
mkdir -p ./.ssh
chmod 0700 ./.ssh
ssh-keygen -t ed25519 -C "kube-dump" -f ./.ssh/kube-dump
cat ./.ssh/kube-dump.pub
kubectl -n kube-dump create secret generic kube-dump-key \
  --from-file=./.ssh/kube-dump \
  --from-file=./.ssh/kube-dump.pub
kubectl create ns kube-dump
kubectl apply -n kube-dump -f deploy/cluster-role-view.yaml
kubectl apply -n kube-dump -f deploy/pvc.yaml
kubectl apply -n kube-dump -f deploy/pod-sa-git-key.yaml
```
