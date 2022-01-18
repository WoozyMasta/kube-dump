FROM alpine:3.13

ARG KUBECTL_VERSION="1.23.1"
ARG JQ_VERSION="1.6"
ARG YQ_VERSION="4.16.2"

LABEL maintainer="woozymasta@gmail.com"

# hadolint ignore=DL3018
RUN apk add --update --no-cache \
    bash openssh-client git tar xz gzip bzip2 curl coreutils grep && \
    curl -sLo /usr/bin/kubectl \
    "https://storage.googleapis.com/kubernetes-release/release/v$KUBECTL_VERSION/bin/linux/amd64/kubectl" && \
    curl -sLo /usr/bin/jq \
    "https://github.com/stedolan/jq/releases/download/jq-$JQ_VERSION/jq-linux64" && \
    curl -sLo /usr/bin/yq \
    "https://github.com/mikefarah/yq/releases/download/v$YQ_VERSION/yq_linux_amd64" && \
    chmod +x /usr/bin/kubectl /usr/bin/jq /usr/bin/yq


COPY ./kube-dump /kube-dump

ENTRYPOINT [ "/kube-dump" ]
