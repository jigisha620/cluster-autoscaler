#!/usr/bin/env bash

# Copyright 2026 The Kubernetes Authors.
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

# Builds the kwok cluster-autoscaler on the host, so go.mod replaces that point
# outside the repo (../kubernetes) work, then packages the binary into an image.
#
# Usage: hack/build-kwok-local.sh [--no-image]
# Env: IMAGE (default cluster-autoscaler-kwok:hints), BASE_IMAGE, OUT_DIR.

set -o errexit
set -o nounset
set -o pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
IMAGE=${IMAGE:-cluster-autoscaler-kwok:hints}
OUT_DIR=${OUT_DIR:-${TMPDIR:-/tmp}/cluster-autoscaler-kwok-local}
CANDIDATES=(
  public.ecr.aws/eks-distro-build-tooling/eks-distro-minimal-base-nonroot:latest-al23
  gcr.io/distroless/static:nonroot
)

mkdir -p "${OUT_DIR}"
(
  cd "${ROOT}"
  CGO_ENABLED=0 GOOS=linux GOARCH=${GOARCH:-amd64} \
    GOTOOLCHAIN=${GOTOOLCHAIN:-local} GOPROXY=${GOPROXY:-direct} GOSUMDB=${GOSUMDB:-off} \
    go build -trimpath -ldflags "-s -w" -o "${OUT_DIR}/cluster-autoscaler-kwok" ./kwok
)
echo "built ${OUT_DIR}/cluster-autoscaler-kwok"

if [[ "${1:-}" == "--no-image" ]]; then
  exit 0
fi

base=${BASE_IMAGE:-}
if [[ -z "${base}" ]]; then
  for c in "${CANDIDATES[@]}"; do
    if docker image inspect "${c}" >/dev/null 2>&1; then base=${c}; break; fi
  done
fi
if [[ -z "${base}" ]]; then
  for c in "${CANDIDATES[@]}"; do
    if docker pull -q "${c}" >/dev/null 2>&1; then base=${c}; break; fi
  done
fi
if [[ -z "${base}" ]]; then
  echo "no base image available; set BASE_IMAGE" >&2
  exit 1
fi

cat > "${OUT_DIR}/Dockerfile" <<DOCKERFILE
FROM ${base}
COPY --chmod=755 cluster-autoscaler-kwok /cluster-autoscaler-kwok
USER 65532:65532
ENTRYPOINT ["/cluster-autoscaler-kwok"]
DOCKERFILE
docker build -q -t "${IMAGE}" "${OUT_DIR}"
echo "built ${IMAGE} from ${base}"
