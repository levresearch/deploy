FROM node:24.21.0-bookworm-slim@sha256:d6aa754f16b3197301076f047b5def2f02ea1dbbc2ca920407d46d7ec7f87b20
RUN apt-get update && apt-get install -y --no-install-recommends qemu-system-x86 qemu-utils cloud-image-utils openssh-client expect curl ca-certificates && rm -rf /var/lib/apt/lists/*
CMD ["sleep", "infinity"]
