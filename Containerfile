# golang:1.26-alpine, pinned to its multi-arch index digest; dependabot bumps both.
FROM docker.io/library/golang:1.26-alpine@sha256:8ac98ca534ac3f51e1f420a1dd2c15e74c75cfa0f23f3ad27eb5d7236c349a0c AS build
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
