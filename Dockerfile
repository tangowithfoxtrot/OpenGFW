# syntax = docker/dockerfile:1.2
###############################################
#                 Build stage                 #
###############################################
FROM golang:1.21-alpine as build

WORKDIR /src

COPY . .

RUN go mod tidy && go build


###############################################
#                  App stage                  #
###############################################
FROM cgr.dev/chainguard/wolfi-base as app

# Disable IPv6; doesn't work in codespaces:
# 2024-01-31T22:41:33Z    FATAL   failed to parse config  {"error": "invalid config: io: running [/usr/sbin/ip6tables -t filter -C FORWARD -m connmark --mark 1001 -j ACCEPT --wait]: exit status 3: modprobe: FATAL: Module ip6_tables not found in directory /lib/modules/6.2.0-1018-azure\nip6tables v1.8.4 (legacy): can't initialize ip6tables table `filter': Table does not exist (do you need to insmod?)\nPerhaps ip6tables or your kernel needs to be upgraded.\n"}
RUN echo 'net.ipv6.conf.all.disable_ipv6 = 1' >> /etc/sysctl.conf \
    && echo 'net.ipv6.conf.default.disable_ipv6 = 1' >> /etc/sysctl.conf \
    && echo 'net.ipv6.conf.lo.disable_ipv6 = 1' >> /etc/sysctl.conf

RUN apk add --no-cache iptables ip6tables && \
    mkdir -p /config

COPY --from=build /src/OpenGFW /usr/local/bin/OpenGFW

ENV OPENGFW_CONFIG_FILE=/config/config.yaml
ENV OPENGFW_LOG_FORMAT=console
ENV OPENGFW_LOG_LEVEL=info
ENV OPENGFW_RULE_FILE=/config/rules.yaml

ENTRYPOINT ["/usr/local/bin/OpenGFW"]
CMD ["${OPENGFW_RULE_FILE}"]
