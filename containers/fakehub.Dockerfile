# fakehub: hermetic huggingface.co simulator for the dev environment and
# e2e tests. Never shipped.
FROM golang:1.26@sha256:ae5a2316d12f3e78fd99177dad452e6ad4f240af2d71d57b480c3477f250fec6 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/fakehub ./cmd/fakehub

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/fakehub /fakehub
EXPOSE 8081
ENTRYPOINT ["/fakehub"]
