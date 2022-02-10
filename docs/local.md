# Run on a local machine

An example of installing kube-dump and dependencies on a local system to work
with kubernetes clusters with which you work through kubectl

## Clone this repo

```shell
git clone https://github.com/WoozyMasta/kube-dump.git
cd kube-dump
```

## Install dependecies

Exemple for Ubuntu

```shell
sudo apt install bash git tar xz-utils gzip bzip2 curl
# kubectl
curl -sLo ~/.local/bin/kubectl \
  https://storage.googleapis.com/kubernetes-release/release/v1.20.2/bin/linux/amd64/kubectl
chmod +x ~/.local/bin/kubectl
# jq
curl -sLo ~/.local/bin/jq \
  https://github.com/stedolan/jq/releases/download/jq-1.6/jq-linux64
chmod +x ~/.local/bin/jq
# yq
curl -sLo ~/.local/bin/yq \
  https://github.com/mikefarah/yq/releases/download/v4.5.0/yq_linux_amd64
chmod +x ~/.local/bin/yq

```

## Run it!

```shell
./kube-dump dump -ca
```
