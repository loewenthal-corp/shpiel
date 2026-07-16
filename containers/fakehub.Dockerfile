# fakehub: hermetic huggingface.co simulator for the dev environment and
# e2e tests. Never shipped.
FROM golang:1.26@sha256:ae5a2316d12f3e78fd99177dad452e6ad4f240af2d71d57b480c3477f250fec6 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/fakehub ./cmd/fakehub

FROM gcr.io/distroless/static-debian12:nonroot@sha256:aef9602f8710ec12bde19d593fed1f76c708531bb7aba205110f1029786ead7b
COPY --from=build /out/fakehub /fakehub
EXPOSE 8081
ENTRYPOINT ["/fakehub"]
