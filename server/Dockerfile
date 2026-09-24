# Multi-stage build for the relay global-session server: a small, static
# binary suitable for a resource-constrained VM (see DEPLOY.md), not a
# general-purpose base image.

FROM golang:1.27-alpine AS builder
WORKDIR /src
# Only this module's own go.mod/go.sum - the root module's tests never run
# in this image, and the two modules are never allowed to share dependencies.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" -o /out/relay-server .

FROM scratch
# CA certs for outbound TLS to a managed Postgres/MongoDB (e.g. Atlas) - the
# only thing scratch is missing that this binary might need.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/relay-server /relay-server

EXPOSE 5555
ENTRYPOINT ["/relay-server"]
