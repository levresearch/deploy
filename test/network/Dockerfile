FROM node:24.21.0-bookworm-slim@sha256:d6aa754f16b3197301076f047b5def2f02ea1dbbc2ca920407d46d7ec7f87b20
RUN apt-get update && apt-get install -y --no-install-recommends qemu-system-x86 qemu-utils cloud-image-utils openssh-client expect curl ca-certificates git && rm -rf /var/lib/apt/lists/*
# the container runs as the invoking user, and ssh needs that uid to have a name
ARG LAB_UID=1000
RUN getent passwd "$LAB_UID" >/dev/null || useradd -u "$LAB_UID" -M -d /lab -s /bin/bash lab
CMD ["sleep", "infinity"]
