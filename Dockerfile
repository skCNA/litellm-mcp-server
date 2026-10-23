FROM --platform=$BUILDPLATFORM golang:1.26.4-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" -o /out/litellm-mcp-server .
RUN mkdir -p /out/data/changes && chmod 0700 /out/data/changes

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/litellm-mcp-server /litellm-mcp-server
COPY --from=builder --chown=65532:65532 /out/data /data

ENV MCP_ADDR=:8080
EXPOSE 8080

ENTRYPOINT ["/litellm-mcp-server"]
