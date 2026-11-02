FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /shipit ./cmd/shipit

FROM alpine:3.22
# Shipit drives the host's Docker through its socket and clones with git.
RUN apk add --no-cache ca-certificates docker-cli docker-cli-buildx git
COPY --from=build /shipit /usr/local/bin/shipit
VOLUME /data
ENTRYPOINT ["shipit"]
