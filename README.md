# bridge-agent

This directory is the seed for the future standalone `bridge-agent` repository.

It contains:

- bridge-agent source
- example config
- agent release packaging script
- GitHub Actions release workflow

The public interface to preserve:

- `agent.yaml` structure
- gateway websocket contract
- tool config fields:
  - `cmd`
  - `args`
  - `continue_args`
  - `working_dir`
