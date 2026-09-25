FROM golang:1.25 AS builder
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /metrics-replayer ./cmd/metrics-replayer

FROM scratch
COPY --from=builder /metrics-replayer /metrics-replayer
EXPOSE 8081
ENTRYPOINT ["/metrics-replayer"]
