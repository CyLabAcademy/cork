# Automated Solvers

A challenge can carry a solve script that proves it is solvable. cork itself
runs no solvers; `ci/examples/run.sh` runs every example's on each pull
request, against a fresh build, and checks that it recovers the flag cork
recorded for that build. A challenge without a solver fails that check unless
it is `flag-only`.

## Usage

To create a solver, add a `solver` directory to the challenge directory with a
`solve.py` script (Python 3) that implements the solution. Unless the
directory brings its own `Dockerfile`, the script runs in Ubuntu 24.04 with
`python3`, `pip3`, `socat` and `build-essential` installed
(`ci/examples/solver.Dockerfile`); anything else goes in the files below.

For a challenge that runs a service, the solver container joins the
instance's network, so it reaches the challenge as `challenge` (and is itself
`solver`) on the port the container listens on, not the one published on the
host.

For artifact-only challenges (no published ports, e.g. `static-make`), there is
no challenge container to connect to: the solver container runs on Docker's
default network, so the `challenge` DNS name does not resolve and the flag must
be recovered from the artifacts. Outbound network access (e.g. for downloading
tooling at runtime) remains available.

The working directory holds the files of the `solver` directory, the artifacts
given to competitors (extracted), and, when the build has lookup values,
`metadata.json` with them.

Once the flag has been retrieved, the solve script should write the flag to a
file named `flag` in the working directory and exit, within ten minutes.

## Installing Dependencies

There are two means for installing additional dependencies that the solver
requires: `packages.txt` for `apt` packages and `requirements.txt` for `pip3`
modules.  Both files consist of a single dependency per line.  Note: although
`requirements.txt` supports pinning the version of each dependency, there is
currently no support for similar functionality in `packages.txt`.
