# Disposable SMB2/3-only Samba fixture for the Movian Go SMB smoke tests.
FROM debian:trixie-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends samba \
 && rm -rf /var/lib/apt/lists/*
COPY smb.conf /etc/samba/smb.conf
COPY entrypoint.sh /entrypoint.sh
RUN chmod 755 /entrypoint.sh && useradd -M -s /usr/sbin/nologin smoke
ENTRYPOINT ["/entrypoint.sh"]
