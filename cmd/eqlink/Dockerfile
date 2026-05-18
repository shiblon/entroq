FROM golang:1.25-bookworm AS builder

ENV CGO_ENABLED=0

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download -x

ARG VERSION=dev
COPY . .
RUN go build -ldflags "-X github.com/shiblon/entroq/pkg/version.injected=${VERSION}" -o /bin/eqlink ./cmd/eqlink

FROM gcr.io/distroless/static-debian12:debug

LABEL org.opencontainers.image.source="https://github.com/shiblon/entroq"

USER 65532:65532

COPY --from=builder /bin/eqlink /bin/eqlink

EXPOSE 8080

ENTRYPOINT ["/bin/eqlink"]
