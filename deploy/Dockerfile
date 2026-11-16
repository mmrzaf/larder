FROM golang:1.27-alpine AS build
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY} \
    GOTOOLCHAIN=local
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=false \
    -ldflags "-s -w -X github.com/mmrzaf/larder/internal/version.Version=${VERSION} -X github.com/mmrzaf/larder/internal/version.Commit=${COMMIT} -X github.com/mmrzaf/larder/internal/version.Date=${DATE}" \
    -o /out/larder ./cmd/larder
RUN mkdir -p /out/data /out/etc/larder

FROM gcr.io/distroless/static-debian12
ARG VERSION=dev
ARG COMMIT=none
LABEL org.opencontainers.image.title="Larder" \
      org.opencontainers.image.description="Self-hosted write-through package cache" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}"
COPY --from=build /out/larder /larder
COPY --from=build --chown=10001:10001 /out/data /data
COPY --from=build /out/etc/larder /etc/larder
USER 10001:10001
ENV LARDER_CONFIG=/etc/larder/larder.yaml
VOLUME ["/data"]
EXPOSE 8080 9090
STOPSIGNAL SIGTERM
ENTRYPOINT ["/larder"]
CMD ["serve"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 CMD ["/larder", "healthcheck"]
