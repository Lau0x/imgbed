FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY templates ./templates
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/imgbed .

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.source=https://github.com/Lau0x/imgbed
LABEL org.opencontainers.image.description="不依赖 PHP 的自托管图床，可直接替换 EasyImage 2.0"
LABEL org.opencontainers.image.licenses=MIT
COPY --from=build /out/imgbed /imgbed
ENV IMGBED_LISTEN=:18321 \
    IMGBED_IMAGES=/data/i \
    IMGBED_TRASH=/data/i-trash \
    IMGBED_TRUSTED_PROXIES=127.0.0.0/8,::1/128,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,fc00::/7
VOLUME /data
EXPOSE 18321
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s CMD ["/imgbed", "healthcheck"]
ENTRYPOINT ["/imgbed"]
