# syntax=docker/dockerfile:1
# NOTE: docs sketched golang:1.23-alpine, but modernc.org/sqlite (CGO-free)
# requires Go >= 1.25, hence 1.25-alpine here.
FROM golang:1.25-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/ecs-phoenix-ext ./cmd/ecs-phoenix-ext

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/ecs-phoenix-ext /ecs-phoenix-ext
USER 65532:65532
EXPOSE 80
ENTRYPOINT ["/ecs-phoenix-ext"]
