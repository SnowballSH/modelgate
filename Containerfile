# golang:1.26-alpine, pinned to its multi-arch index digest; dependabot bumps both.
FROM docker.io/library/golang:1.27-alpine@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /modelgate ./cmd/modelgate

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /modelgate /modelgate
USER 65532:65532
ENTRYPOINT ["/modelgate"]
