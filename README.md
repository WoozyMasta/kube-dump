# Kube-dump <!-- omit in toc --> 

Backup a Kubernetes cluster as a yaml manifest.

![Logo](https://raw.githubusercontent.com/WoozyMasta/kube-dump/master/extras/logo-wide.png)

* [Description](#description)
* [Quick Start Guides](#quick-start-guides)
* [Dependencies](#dependencies)
* [Commands and flags](#commands-and-flags)
* [Environment variables](#environment-variables)

## Description

With this utility you can save your cluster resources as nice yaml manifests without unnecessary metadata.

Key features:

* Saving only those resources to which you have read access;
* Can work with a list of namespaces otherwise all available ones will be used;
* Can save both namespaced and cluster wide resources;
* You can run locally, in a container or in a cluster;
* Can archive and rotate dump archives;
* Can commit dumps to a git repository and send to a remote repository;
* You can specify a list of resources to be dumped;
* It is possible to configure via command line arguments as well as via environment variables.

Plans to implement:

* Sending dumps to s3 bucket;
* Sending notifications by email and webhook;
* Git-crypt to encrypt secrets
* Bash autocomplete 

---

[![asciicast](https://asciinema.org/a/DEOjycqfHNa8Rrietk3mbaPvp.svg)](https://asciinema.org/a/DEOjycqfHNa8Rrietk3mbaPvp)

## Quick Start Guides 
* [Run on a local machine](./docs/local.md) (dependencies and a config for kubectl are required)
* [Run in container](./docs/container.md) (docker, podman, etc. required and a config for kubectl)
* [Run in kubernetes as pod](./docs/pod.md) (requires access to the kubernetes cluster and config for kubectl)
* [Run in kubernetes as a cron job using a service account](./docs/conjob.md) (requires access to the kubernetes cluster and the ability to create a role or cluster role) 

## Dependencies

Required dependencies:

* [kubectl](https://kubernetes.io/docs/tasks/tools/install-kubectl/) - Kubernetes command-line tool
* [jq](https://github.com/stedolan/jq) - Command-line JSON processor
* [yq](https://github.com/mikefarah/yq) - Command-line YAML processor

Optional dependencies:

* curl - Used to check kubernetes api livez probe when use serviceaccount
* git - Used to store backups as a git repository
* tar - Used to create backup archives with one of the compression libraries:
  * xz - a lossless data compression file format based on the LZMA algorithm
  * gzip - single-file/stream lossless data compression utility
  * bzip2 - compression program that uses the Burrows–Wheeler algorithm

## Commands and flags

```
./kube-dump [command] [[flags]]

Available Commands:
  dump-namespaces    Dump namespaced kubernetes resources
  dump-cluster       Dump cluster wide kubernetes resources
  dump               Dump all kubernetes resources

Flags:
  -h, --help                    This help
  -s, --silent                  Execute silently, suppress all stdout messages
  -d, --destination-dir         Path to dir for store dumps, default ./data
  -f, --force-remove            Delete resources data directory before launch

Kubernetes flags:
  -n, --namespaces              List of kubernetes namespaces
  -r, --namespaced-resources    List of namespaced resources
  -k, --cluster-resources       List of cluster resources
      --kube-config             Path to kubeconfig file
      --kube-context            The name of the kubeconfig context to use
      --kube-cluster            The name of the kubeconfig cluster to use
      --kube-insecure-tls       Skip check server's certificate for validity

Git commit flags:
  -c, --git-commit              Commit changes
  -p, --git-push                Commit changes and push to origin
  -b, --git-branch              Branch name
      --git-commit-user         Commit author username
      --git-commit-email        Commit author email
      --git-remote-name         Remote repo name, defualt is origin
      --git-remote-url          Remote repo URL

Archivate flags:
  -a, --archivate               Create archive of data dir
      --archive-rotate-days     Rotate archives older than N days
      --archive-type            Archive type xz, gz or bz2, default is tar
```

## Environment variables

All environment variables are described in the [.env](./.env) file, you can use them both for the container launch configuration and directly from the [`.env`](./.env) file, it is read automatically at startup.
