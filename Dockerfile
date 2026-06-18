FROM golang:1.25 AS builder

WORKDIR /app

COPY . .

RUN go mod download

RUN CGO_ENABLED=0 GOOS=linux \
    go build -ldflags="-s -w" \
    -o /ainovel \
    ./cmd/ainovel-cli

FROM alpine:latest

RUN apk add --no-cache \
    ca-certificates \
    tzdata

WORKDIR /workspace

COPY --from=builder /ainovel /usr/local/bin/ainovel

ENV TZ=Asia/Shanghai

CMD ["tail","-f","/dev/null"]
