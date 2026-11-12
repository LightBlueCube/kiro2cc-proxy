# syntax=docker/dockerfile:1

# agyrelay image. It ships no agy: on start, the entrypoint downloads the
# pinned agy release into the data volume when the volume has none or this
# agyrelay cannot run it.
#
# Build, from the repository root:
#   docker build -f deploy/Dockerfile -t agyrelay .
#
# Run (IMAGE: e.g. ghcr.io/OWNER/REPO:BRANCH):
#   docker run -d --name agyrelay --restart unless-stopped \
#     -p 127.0.0.1:8787:8787 -v agyrelay:/var/lib/agyrelay IMAGE
#   docker exec -it agyrelay agyrelay passwd -config /etc/agyrelay.json
#   docker logs -f agyrelay
#
# Put a TLS reverse proxy in front of port 8787; the admin UI is at /admin/
# and signs in over HTTPS only. The download honors https_proxy
# (-e https_proxy=http://HOST:PORT).
#
# /etc/agyrelay.json (deploy/container.json) trusts the forwarding headers
# (X-Forwarded-For, CF-Connecting-IP, X-Forwarded-Proto) of loopback and of
# Docker's bridge networks, 172.16.0.0/12: Docker's port forwarding from
# the host's loopback, and proxies in other containers, connect from there.
# To change that, mount your own config, keeping data_dir and agy_bin:
#   -v ./agyrelay.json:/etc/agyrelay.json:ro

FROM golang:1.27.3-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
	deploy/build.sh /out/agyrelay

FROM debian:trixie-slim
RUN apt-get update \
	&& apt-get install -y --no-install-recommends ca-certificates curl \
	&& rm -rf /var/lib/apt/lists/*
# Fixed ids: the data volume keeps its owner across images.
RUN groupadd --gid 10001 agyrelay \
	&& useradd --uid 10001 --gid agyrelay --no-log-init --no-create-home \
		--home-dir /var/lib/agyrelay --shell /usr/sbin/nologin agyrelay \
	&& install -d -o agyrelay -g agyrelay -m 0700 /var/lib/agyrelay
COPY --from=build /out/agyrelay /usr/local/bin/agyrelay
COPY deploy/fetch-agy.sh deploy/docker-entrypoint.sh /usr/local/lib/agyrelay/
COPY deploy/container.json /etc/agyrelay.json
USER 10001:10001
VOLUME /var/lib/agyrelay
EXPOSE 8787
ENTRYPOINT ["/usr/local/lib/agyrelay/docker-entrypoint.sh"]
