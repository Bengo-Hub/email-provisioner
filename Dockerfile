# syntax=docker/dockerfile:1
FROM golang:1.23-alpine AS builder
WORKDIR /src
RUN apk add --no-cache git ca-certificates
COPY go.mod go.sum ./
RUN GOTOOLCHAIN=auto go mod download
COPY . .
RUN GOTOOLCHAIN=auto CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /bin/email-provisioner ./cmd/api

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && addgroup -S app && adduser -S app -G app
WORKDIR /app
COPY --from=builder /bin/email-provisioner /usr/local/bin/email-provisioner
USER app
EXPOSE 8080
ENV PORT=8080
ENTRYPOINT ["/usr/local/bin/email-provisioner"]
