FROM alpine:3.18

ARG TARGETARCH

ARG KUBECTL_VERSION="1.26.6"

LABEL maintainer="marlon.costa@datacosmos.com.br"

# hadolint ignore=DL3018
RUN apk add --update --no-cache \
    bash bind-tools jq yq openssh-client git tar xz gzip bzip2 curl coreutils grep && \
    curl -sLo /usr/bin/kubectl \
    "https://storage.googleapis.com/kubernetes-release/release/v$KUBECTL_VERSION/bin/linux/$TARGETARCH/kubectl" && \
    chmod +x /usr/bin/kubectl

COPY ./kube-dump /kube-dump

ENTRYPOINT [ "/kube-dump" ]
