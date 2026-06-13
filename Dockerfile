FROM public.ecr.aws/docker/library/golang:1.26-bookworm AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=docker
RUN CGO_ENABLED=0 go build \
    -ldflags "-s -w -X git.erwanleboucher.dev/eleboucher/forgesync/internal/version.Version=${VERSION}" \
    -trimpath \
    -o /forgesync \
    ./cmd/forgesync

FROM alpine:3.24

WORKDIR /forgesync

RUN apk add --no-cache git

COPY --from=builder /forgesync /forgesync/forgesync

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --retries=3 \
  CMD ["forgesync", "healthcheck"]

ENTRYPOINT ["/forgesync/forgesync"]
CMD ["run"]
