# 单阶段:target/app.jar 由构建脚本(ci/build.sh → mvn package)先生成,这里只拷进 JRE 镜像。
FROM mirror.gcr.io/library/eclipse-temurin:21-jre
ARG VERSION=dev
ARG PROFILE=default
COPY target/app.jar /app/app.jar
RUN printf '%s' "$VERSION" > /app/version.txt
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["java", "-jar", "/app/app.jar"]
