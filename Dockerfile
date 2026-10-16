FROM golang:1.21-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /out/checkin-server .

FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S checkin \
    && adduser -S -G checkin -H -s /sbin/nologin checkin

COPY --from=builder /out/checkin-server /usr/local/bin/checkin-server

ENV TZ=Asia/Shanghai
USER checkin
WORKDIR /app
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/checkin-server"]
