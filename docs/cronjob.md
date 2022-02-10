# Run in kubernetes as a cron job using a service account <!-- omit in toc -->

Here's how to set up a cluster backup using a CronJob and a service account.

You can read about CronJobs in [doc](https://kubernetes.io/docs/concepts/workloads/controllers/cron-jobs/)

* [Create namespace](#create-namespace)
* [Deploy serviceaccount](#deploy-serviceaccount)
* [Deploy with git repository oauth token](#deploy-with-git-repository-oauth-token)
* [Deploy with git repository write allowed ssh key](#deploy-with-git-repository-write-allowed-ssh-key)
* [Deploy with persistent volume to store achives](#deploy-with-persistent-volume-to-store-achives)

## Create namespace

First, let's create a namespace kube-dump (if you want to use a different name,
you will need to edit it in all the necessary manifests)

```shell
kubectl create ns kube-dump
```

## Deploy serviceaccount

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
kubectl apply -n kube-dump -f deploy/cluster-role-view.yaml
```

## Deploy with git repository oauth token

If you want to use a git repository to store dumps, you will need to provide the
address of the remote repository and credentials.

Let's create a secret with the `GIT_REMOTE_URL` variable in which we will
specify the https repository address. Insert your oauth token in place of the
`$TOKEN` variable:

```shell
kubectl -n kube-dump create secret generic kube-dump \
  --from-literal=GIT_REMOTE_URL=https://oauth2:$TOKEN@corp-gitlab.com/devops/cluster-bkp.git
```

And apply the cron job manifest:

```shell
kubectl apply -n kube-dump -f deploy/cronjob-git-token.yaml
```

## Deploy with git repository write allowed ssh key

Generate ssh key:

```shell
mkdir -p ./.ssh
chmod 0700 ./.ssh
ssh-keygen -t ed25519 -C "kube-dump" -f ./.ssh/kube-dump
cat ./.ssh/kube-dump.pub
```

Show public ssh key and add to repository deployment keys with write access.

```shell
cat ./.ssh/kube-dump.pub
```

Create pvc for store data such as cache

```shell
kubectl apply -n kube-dump -f deploy/pvc.yaml
```

Create secret with private ssh key

```shell
kubectl -n kube-dump create secret generic kube-dump-key \
  --from-file=./.ssh/kube-dump \
  --from-file=./.ssh/kube-dump.pub
```

And apply the cron job manifest,
previously you could set up environment variables

```shell
kubectl apply -n kube-dump -f deploy/cronjob-git-key.yaml
```

## Deploy with persistent volume to store achives

If you want to use archives saved to disk for storing dumps,
you will need to create a PVC and connect it to a cron job.

```shell
kubectl apply -n kube-dump -f deploy/pvc.yaml
```

And apply the cron job manifest:

```shell
kubectl apply -n kube-dump -f deploy/cronjob-archives.yaml
```
