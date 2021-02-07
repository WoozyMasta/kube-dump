# Run in kubernetes as a cron job using a service account

## TODO
```shell
kubectl -n kube-dump create secret generic kube-dump \
  --from-literal=GIT_REMOTE_URL=https://oauth2:$TOKEN@corp-gitlab.com/devops/cluster-bkp.git
```
