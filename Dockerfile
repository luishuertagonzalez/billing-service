# Build stage
FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o server ./cmd/server/

# Runtime stage
FROM gcr.io/distroless/static-debian11
COPY --from=builder /app/server /server
EXPOSE 8084
CMD ["/server"]
