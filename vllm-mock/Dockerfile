# Multi-stage build. Target is distroless static (nonroot), builds for
# the OCI Ampere A1 (arm64) by default; override via --platform.
ARG GO_VERSION=1.23
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags='-s -w' -o /out/vllm-mock .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/vllm-mock /vllm-mock
EXPOSE 8000
USER nonroot:nonroot
ENTRYPOINT ["/vllm-mock"]
