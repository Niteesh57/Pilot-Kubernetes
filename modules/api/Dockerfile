# Copyright 2017 The Kubernetes Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

FROM alpine:3.21 AS user

ENV USER=nonroot
ENV UID=10001

# Create nonroot user and install CA certificates.
RUN apk add --no-cache ca-certificates && \
    adduser \
    --disabled-password \
    --gecos "" \
    --home "/nonexistent" \
    --shell "/sbin/nologin" \
    --no-create-home \
    --uid "${UID}" \
    "${USER}"

FROM golang:1.24-alpine3.22 AS builder

ARG TARGETARCH=amd64
ARG TARGETOS=linux
ARG VERSION=latest

WORKDIR /workspace

# Copy required local modules
COPY /modules/common/certificates /workspace/common/certificates
COPY /modules/common/client /workspace/common/client
COPY /modules/common/csrf /workspace/common/csrf
COPY /modules/common/errors /workspace/common/errors
COPY /modules/common/types /workspace/common/types
COPY /modules/common/helpers /workspace/common/helpers

WORKDIR /workspace/api

# Retrieve application dependencies.
# This allows the container build to reuse cached dependencies.
# Expecting to copy go.mod and if present go.sum.
COPY /modules/api/go.* ./
COPY /modules/api/pkg ./pkg
COPY /modules/api/main.go ./
RUN go mod tidy
RUN go mod download

RUN echo ${VERSION}
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags="-s -w -X k8s.io/dashboard/api/pkg/environment.Version=${VERSION}" -o dashboard-api .

# Scratch can be used as the base image because the binary is compiled to include all dependencies.
FROM scratch AS final

COPY --from=builder /workspace/api/dashboard-api /dashboard-api

# Import the user and group files and CA certificates from the builder.
COPY --from=user /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=user /etc/passwd /etc/passwd
COPY --from=user /etc/group /etc/group

USER nonroot:nonroot

# The port that the application listens on.
EXPOSE 8000 8001
ENTRYPOINT ["/dashboard-api", "--insecure-bind-address=0.0.0.0", "--bind-address=0.0.0.0"]
