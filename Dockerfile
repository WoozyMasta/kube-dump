# syntax=docker/dockerfile:1

ARG GO_VERSION=1.27
FROM --platform=$BUILDPLATFORM docker.io/library/golang:$GO_VERSION AS build

ARG TARGETOS
ARG TARGETARCH
ARG CONTAINER_IMAGE

WORKDIR /src

COPY go.mod go.sum Makefile ./
RUN make download tool-schemadoc

COPY . .
RUN make generate compile \
  BUILD_GOOS="$TARGETOS" \
  BUILD_GOARCH="$TARGETARCH" \
  CONTAINER_IMAGE="$CONTAINER_IMAGE" && \
  install -d -m 1777 /runtime-tmp

FROM scratch

COPY --from=build /runtime-tmp /tmp
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /src/build/kube-dump /kube-dump

USER 65532:65532

ENTRYPOINT ["/kube-dump"]
CMD ["version"]
