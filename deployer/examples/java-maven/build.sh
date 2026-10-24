#!/bin/sh
# 构建脚本:在 maven 镜像里运行,工作目录是 src/java-maven。PROFILE 选择 Maven profile(dev/prod)。
set -eu
# 沙箱出口有 TLS 中间人:把额外 CA 导入一份 JDK 信任库副本给 Maven 用(没有该文件就跳过)
if [ -n "${BUILD_EXTRA_CA_FILE:-}" ] && [ -f "$BUILD_EXTRA_CA_FILE" ]; then
  cp "$JAVA_HOME/lib/security/cacerts" /tmp/cacerts && chmod u+w /tmp/cacerts
  awk '/BEGIN CERTIFICATE/{n++} {print > "/tmp/extra-ca-" n ".pem"}' "$BUILD_EXTRA_CA_FILE"
  for f in /tmp/extra-ca-*.pem; do
    keytool -importcert -noprompt -trustcacerts -alias "extra-$(basename "$f" .pem)" -file "$f" -keystore /tmp/cacerts -storepass changeit >/dev/null 2>&1 || true
  done
  export MAVEN_OPTS="-Djavax.net.ssl.trustStore=/tmp/cacerts -Djavax.net.ssl.trustStorePassword=changeit"
fi
mvn -B -ntp -q -P"${PROFILE:-dev}" -Dbuild.version="$VERSION" package
ls -l target/app.jar
