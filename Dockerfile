FROM golang:1.23 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go tools.json ./
RUN CGO_ENABLED=0 go build -trimpath -o /out/bot-mcp .

FROM alpine:3.23
RUN apk add --no-cache ca-certificates \
    && addgroup -S app && adduser -S -G app app \
    && mkdir -p /app && chown app:app /app
COPY --from=build /out/bot-mcp /bot-mcp
WORKDIR /app
USER app
EXPOSE 65460
ENTRYPOINT ["/bot-mcp"]
