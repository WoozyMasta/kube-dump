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

For more convenience, you can create an alias for calling kube-dump from a container:

```shell
alias kube-dump='docker run --tty --interactive --rm \
  --volume $HOME/.kube:/.kube --volume $HOME/dump:/dump \
  --env KUBE_CONFIG=/.kube/config --env DESTINATION_DIR=/dump \
  woozymasta/kube-dump:latest'
```

Add this alias to your `~/.bashrc` or `~/.bash_aliases` so as not to lose the command.

Now you can just call `kube-dump dump-namespaces -n dev` and watch the resource dumps from the dev namespace in the `~/dump` directory.

All environment variables are described in the [.env](../.env) file
