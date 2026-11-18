ARG GO_VERSION
FROM golang:${GO_VERSION}

ARG UID=1000
RUN apt-get update \
    && apt-get install -y --no-install-recommends nodejs npm jq python3 ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --uid "${UID}" --shell /bin/bash validator
USER validator
RUN git config --global user.name "Detent validation" \
    && git config --global user.email "validation@example.invalid" \
    && git config --global --add safe.directory '*'
