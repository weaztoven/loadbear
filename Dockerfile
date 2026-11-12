FROM golang:1.22 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-X github.com/weaztoven/loadbear/internal/version.Version=${VERSION} -X github.com/weaztoven/loadbear/internal/version.Commit=${COMMIT} -X github.com/weaztoven/loadbear/internal/version.BuildDate=${BUILD_DATE}" \
    -o /out/loadbear ./cmd/loadbear \
    && CGO_ENABLED=0 go build -trimpath \
    -ldflags "-X github.com/weaztoven/loadbear/internal/version.Version=${VERSION} -X github.com/weaztoven/loadbear/internal/version.Commit=${COMMIT} -X github.com/weaztoven/loadbear/internal/version.BuildDate=${BUILD_DATE}" \
    -o /out/fakebackend ./cmd/fakebackend

FROM gcr.io/distroless/static-debian12:nonroot AS fakebackend
COPY --from=build /out/fakebackend /fakebackend
ENTRYPOINT ["/fakebackend"]

FROM gcr.io/distroless/static-debian12:nonroot AS loadbear
COPY --from=build /out/loadbear /loadbear
ENTRYPOINT ["/loadbear"]
CMD ["--config", "/etc/loadbear/config.yaml"]
EXPOSE 8080 9090
