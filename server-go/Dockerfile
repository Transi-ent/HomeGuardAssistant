FROM golang:1.22 AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/homeguard-server .

FROM alpine:3.20
WORKDIR /app
COPY --from=build /out/homeguard-server /app/homeguard-server
EXPOSE 8080
ENTRYPOINT ["/app/homeguard-server"]
