# shipd artifact image.
#
# This image is not how shipd runs (it runs as a host systemd service — it
# needs the docker CLI and a real HOME for git). The image is the *artifact
# carrier*: the local registry is the update channel, and update.sh extracts
# /shipd from this image, swaps it into /usr/local/bin, and restarts.
FROM golang:1.24-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY dashboard.html ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/shipd .

FROM scratch
COPY --from=build /out/shipd /shipd

CMD ["/shipd"]
