# Build the shpiel binary: static, CGO-free, distroless runtime.
# Multi-arch: the build stage runs on the build host's platform and
# cross-compiles for $TARGETARCH, so arm64 images do not pay the QEMU tax.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
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

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/shpiel /shpiel
EXPOSE 8080 9090
ENTRYPOINT ["/shpiel"]
CMD ["serve", "--config", "/etc/shpiel/config.yaml"]
