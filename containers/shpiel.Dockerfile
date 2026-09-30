# Build the shpiel binary: static, CGO-free, distroless runtime.
# Multi-arch: the build stage runs on the build host's platform and
# cross-compiles for $TARGETARCH, so arm64 images do not pay the QEMU tax.
FROM --platform=$BUILDPLATFORM golang:1.26@sha256:0f063af2d465d8dcae54cce04278ada488b96f77b42449c8d071e47d016cc65a AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG GIT_COMMIT=unknown
ARG BUILD_TIME=unknown
ARG VERSION=
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -ldflags "-X github.com/loewenthal-corp/shpiel/internal/buildinfo.Commit=${GIT_COMMIT} -X github.com/loewenthal-corp/shpiel/internal/buildinfo.BuildTime=${BUILD_TIME} ${VERSION:+-X github.com/loewenthal-corp/shpiel/internal/buildinfo.Version=${VERSION}}" \
    -o /out/shpiel ./cmd/shpiel

FROM gcr.io/distroless/static-debian12:nonroot@sha256:aef9602f8710ec12bde19d593fed1f76c708531bb7aba205110f1029786ead7b
COPY --from=build /out/shpiel /shpiel
EXPOSE 8080 9090
ENTRYPOINT ["/shpiel"]
CMD ["serve", "--config", "/etc/shpiel/config.yaml"]
