FROM --platform=$BUILDPLATFORM node:26.13.1-alpine@sha256:6bd9ab43c0ec0190ca7b2d193906cb8b7edf6e7f4afa43ddeccd6452710bffb5 AS frontend
WORKDIR /src
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run check && npm run build

FROM --platform=$BUILDPLATFORM golang:1.27.4-alpine@sha256:0a5f7fe796599ff487605987432e7619ae45beeb2fba4c2ce0b907879932aebf AS backend
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY assets.go ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY locales/ ./locales/
COPY --from=frontend /src/dist/ ./web/dist/
ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags="-s -w -X main.version=$VERSION" -o /out/subtitle-translator ./cmd/subtitle-translator

FROM alpine:3.24.4@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf
RUN apk add --no-cache ca-certificates tzdata su-exec \
    && addgroup -S -g 1000 app && adduser -S -D -H -u 1000 -G app app \
    && mkdir -p /app/data && chown app:app /app/data
WORKDIR /app
ENV DATA_DIR=/app/data LISTEN_ADDR=:8000
COPY --from=backend /out/subtitle-translator /app/subtitle-translator
COPY --chmod=755 docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
EXPOSE 8000
VOLUME ["/app/data"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/app/subtitle-translator", "-healthcheck"]
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["/app/subtitle-translator"]
