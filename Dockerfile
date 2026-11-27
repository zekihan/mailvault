FROM --platform=$BUILDPLATFORM golang:1.27.4-bookworm@sha256:ce59b41415e2524ae99926fe1f37ce48ba8c194dcbd2a56a65436cdcefa2bd34 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /mailvault ./cmd/mailvault

FROM debian:bookworm-slim@sha256:e67eeead736ff7bbe258ef088bff683635c561b1fcfefad0806c1df4ab49ce0e
LABEL org.opencontainers.image.title="mailvault" \
      org.opencontainers.image.description="Back up mail from multiple sources to multiple targets." \
      org.opencontainers.image.source="https://github.com/zekihan/mailvault" \
      org.opencontainers.image.url="https://github.com/zekihan/mailvault" \
      org.opencontainers.image.licenses="AGPL-3.0-only"
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates openssl \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 65532 nonroot \
    && useradd --uid 65532 --gid 65532 --no-log-init --create-home --home-dir /home/nonroot --shell /usr/sbin/nologin nonroot \
    && install -d -m 0700 -o 65532 -g 65532 /state /backups
COPY --from=build /mailvault /usr/local/bin/mailvault
USER 65532:65532
ENTRYPOINT ["mailvault"]
CMD ["--help"]
