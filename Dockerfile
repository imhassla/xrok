FROM golang:1.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/xrok .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/xrok /xrok
USER nonroot:nonroot
ENTRYPOINT ["/xrok"]
