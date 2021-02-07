# Run in container

An example of running kube-dump in a container to work with kubernetes clusters that you work with locally via kubectl.

Startup example for dump namespaces **dev** and **prod** in `$HOME/dump` directory:

```shell
docker run --tty --interactive --rm \
  --volume $HOME/.kube:/.kube --volume $HOME/dump:/dump \
  woozymasta/kube-dump:latest \
  dump-namespaces -n dev,prod -d /dump --kube-config /.kube/config
```

Kube-dump is set as entrypoint, you only need to pass command and flags to container.
