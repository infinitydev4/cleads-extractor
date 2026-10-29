# syntax=docker/dockerfile:1.7

ARG GO_VERSION=1.27

FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -ldflags="-s -w -X main.version=${VERSION}" -o /out/cleads-extractor ./cmd/server

# distroless/static: no shell, no package manager, CA certificates and tzdata
# included, runs as an unprivileged user. Final image ≈ 20 MB.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/cleads-extractor /usr/local/bin/cleads-extractor
ENV PORT=8080
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/cleads-extractor"]
