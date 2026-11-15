FROM golang:1.25.5 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/billgod ./cmd/billgod && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/billgo ./cmd/billgo

FROM alpine:3.20
# ssh 客户端用于 ssh 方式唤醒；docker CLI 用于 local 方式唤醒。
# 不需要 local 唤醒时可以 --build-arg WITH_DOCKER_CLI=0 去掉 docker CLI。
ARG WITH_DOCKER_CLI=1
RUN apk add --no-cache openssh-client && \
    if [ "$WITH_DOCKER_CLI" = "1" ]; then apk add --no-cache docker-cli; fi
COPY --from=build /out/billgod /out/billgo /usr/local/bin/
COPY scripts/wake.sh /opt/billgo/wake.sh
RUN chmod +x /opt/billgo/wake.sh
VOLUME /data
ENTRYPOINT ["billgod"]
CMD ["-config", "/etc/billgo/config.json"]
