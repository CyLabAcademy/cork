# The image a solver runs in when its solver/ directory brings no Dockerfile
# of its own (examples/solvers.md describes the contract).
FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y \
    python3-pip \
    socat \
    build-essential && \
    rm -rf /var/lib/apt/lists/*

COPY Dockerfile packages.txt* ./
RUN if [ -f packages.txt ]; then apt-get update && xargs -a packages.txt apt-get install -y && rm -rf /var/lib/apt/lists/*; fi

COPY Dockerfile requirements.txt* ./
RUN if [ -f requirements.txt ]; then pip3 install --break-system-packages -r requirements.txt; fi

COPY . /solve
WORKDIR /solve

CMD ["python3", "solve.py"]
