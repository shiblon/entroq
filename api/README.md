# Protocol Buffers

To build new proto code after changing proto files, run `./protoc.sh` in this directory.

It will attempt to build a local dockerfile containing the appropriate protoc
tools and plugin versions if it is not available at `entroq-goprotoc:local`. It
then mounts this directory and runs the compilation inside the container.

If the container is already built, it will skip that step and just run the
tools, so it's only slow on the first invocation.

Note that if you want proto generated files to be updated for other languages,
look for relevant directories under the languages in question. Run those
scripts, and they work the same way.
