FROM node:lts-buster-slim

# Install, then run pbjs to get its own dependencies, too.
RUN npm install -g --save --save-prefix=~ \
      protobufjs@6.10.2 \
      semver@7.1.2 \
      chalk@4.0.0 \
      glob@7.1.6 \
      jsdoc@3.6.3 \
      minimist@1.2.0 \
      tmp@0.2.0 \
      uglify-js@3.7.7 \
      espree@7.0.0 \
      escodegen@2.0.0 \
      estraverse@5.1.0

ENTRYPOINT []
WORKDIR /src
VOLUME /src

