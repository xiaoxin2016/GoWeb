FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /goweb .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=build /goweb /app/goweb
ENV GOWEB_DATA_DIR=/app/data
VOLUME /app/data
EXPOSE 8080
ENTRYPOINT ["/app/goweb"]
