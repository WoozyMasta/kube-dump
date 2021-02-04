FROM alpine:edge

RUN printf '%s\n' 'https://dl-cdn.alpinelinux.org/alpine/edge/testing' \
      >> /etc/apk/repositories && \
    apk add --update --no-cache bash jq yq git tar xz gzip bzip2 kubectl

COPY ./kube-dump /kube-dump

ENTRYPOINT [ "/kube-dump" ]
