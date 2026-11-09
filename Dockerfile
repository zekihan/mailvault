FROM --platform=$BUILDPLATFORM golang:1.27.3-bookworm@sha256:dc0963f32053e4bc451d24d87e3892d42d01021b61f093e98eb95c0352bb84b0 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/root/.cache/go-build CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /mailvault ./cmd/mailvault

FROM debian:bookworm-slim@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates openssl \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 65532 nonroot \
    && useradd --uid 65532 --gid 65532 --no-log-init --create-home --home-dir /home/nonroot --shell /usr/sbin/nologin nonroot \
    && install -d -m 0700 -o 65532 -g 65532 /state /backups
COPY --from=build /mailvault /usr/local/bin/mailvault
USER 65532:65532
ENTRYPOINT ["mailvault"]
CMD ["--help"]
