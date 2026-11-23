FROM node:26.13.1-bookworm-slim@sha256:5df79989ed685e09f0a98019db4cebfb22fb9df1a53c0161664a011f7ff6b0f1
RUN apt-get update && apt-get install -y --no-install-recommends qemu-system-x86 qemu-utils cloud-image-utils openssh-client expect curl ca-certificates git && rm -rf /var/lib/apt/lists/*
# the container runs as the invoking user, and ssh needs that uid to have a name
ARG LAB_UID=1000
RUN getent passwd "$LAB_UID" >/dev/null || useradd -u "$LAB_UID" -M -d /lab -s /bin/bash lab
CMD ["sleep", "infinity"]
