FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o jarvis ./cmd/jarvis

FROM alpine:latest
RUN apk --no-cache add ca-certificates tzdata
WORKDIR /app
COPY --from=builder /app/jarvis .
COPY docs/ docs/
EXPOSE 8080
CMD ["./jarvis"]
