# Both multi-platform indexes were resolved and reviewed on 2026-10-27.
FROM --platform=$BUILDPLATFORM golang:1.27.3-bookworm@sha256:972b4110ba0f47539a975c31e52c64765e1ff19f2d74469ae4a595c6676e80a5 AS build
WORKDIR /src
ENV GOTOOLCHAIN=local
COPY mise.toml mise.lock go.mod go.sum ./
COPY tools/check-go.sh tools/check-go.sh
# The base supplies the compiler; drift from any repository pin is an error.
RUN sh tools/check-go.sh && go mod download
COPY cmd/ cmd/
COPY internal/ internal/
ARG TARGETOS
ARG TARGETARCH
RUN test "$TARGETOS" = linux && \
    case "$TARGETARCH" in amd64|arm64) ;; *) exit 1 ;; esac && \
    CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" \
    go build -mod=readonly -trimpath -buildvcs=false -o /out/near-e2ee-proxy ./cmd/near-e2ee-proxy

# CA certificates and timezone data are present; no shell or dynamic libc needed.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build --chmod=0555 /out/near-e2ee-proxy /near-e2ee-proxy
USER 65532:65532
ENV LISTEN_ADDR=0.0.0.0:8080
EXPOSE 8080
STOPSIGNAL SIGTERM
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 CMD ["/near-e2ee-proxy", "-healthcheck"]
ENTRYPOINT ["/near-e2ee-proxy"]
