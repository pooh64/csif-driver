FROM ubuntu
LABEL description="iscsi-test"
RUN apt-get update && \
apt-get install -y iproute2 iputils-ping vim && apt-get install -y tgt open-iscsi && \
apt-get autoclean -y && apt-get autoremove -y && rm -rf /var/lib/apt-get/lists/*
ADD scripts ./scripts