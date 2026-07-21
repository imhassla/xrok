FROM golang:1.24.1-alpine3.21 AS build
WORKDIR /src
COPY main.go .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /natprobe main.go

FROM alpine:3.21
RUN apk add --no-cache iproute2
COPY --from=build /natprobe /usr/local/bin/natprobe
ENTRYPOINT ["/bin/sh", "-c"]
