FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY templates ./templates
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -tags nodynamic -trimpath -ldflags="-s -w" -o /out/easyimage-go . \
    && mkdir -p /out/data/i /out/data/i-trash /out/data/config

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.source=https://github.com/Lau0x/easyimage-go
LABEL org.opencontainers.image.description="不依赖 PHP 的自托管图床，可直接替换 EasyImage 2.0"
LABEL org.opencontainers.image.licenses=MIT
COPY --from=build /out/easyimage-go /easyimage-go
# 不挂载目录直接 docker run 时，匿名卷沿用这里的属主，非 root 用户也能写入
COPY --from=build --chown=65532:65532 /out/data /data
ENV EASYIMAGE_LISTEN=:18321 \
    EASYIMAGE_IMAGES=/data/i \
    EASYIMAGE_TRASH=/data/i-trash \
    EASYIMAGE_CONFIG_DIR=/data/config \
    EASYIMAGE_TRUSTED_PROXIES=127.0.0.0/8,::1/128,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,fc00::/7
VOLUME /data
EXPOSE 18321
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s CMD ["/easyimage-go", "healthcheck"]
ENTRYPOINT ["/easyimage-go"]
