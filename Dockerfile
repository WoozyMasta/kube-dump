FROM alpine:edge

LABEL maintainer="woozymasta@gmail.com"

RUN printf '%s\n' 'https://dl-cdn.alpinelinux.org/alpine/edge/testing' \
      >> /etc/apk/repositories && \
    apk add --update --no-cache bash jq yq git tar xz gzip bzip2 kubectl curl

COPY ./kube-dump /kube-dump

ENTRYPOINT [ "/kube-dump" ]
