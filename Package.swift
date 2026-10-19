// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "CodeLinkEngine",
    platforms: [.macOS(.v15)],
    products: [.library(name: "CodeLinkEngine", targets: ["CodeLinkEngine"])],
    dependencies: [
        .package(url: "https://github.com/hummingbird-project/hummingbird.git", from: "2.0.0"),
        .package(url: "https://github.com/apple/swift-nio.git", from: "2.80.0"),
        .package(url: "https://github.com/apple/swift-log.git", from: "1.6.0"),
        .package(url: "https://github.com/swift-server/swift-service-lifecycle.git", from: "2.6.0"),
        .package(url: "https://github.com/apple/swift-http-types.git", from: "1.0.0"),
    ],
    targets: [
        .target(name: "CodeLinkEngine", dependencies: [
            .product(name: "Hummingbird", package: "hummingbird"),
            .product(name: "NIOCore", package: "swift-nio"),
            .product(name: "Logging", package: "swift-log"),
            .product(name: "ServiceLifecycle", package: "swift-service-lifecycle"),
            .product(name: "HTTPTypes", package: "swift-http-types"),
        ]),
        .testTarget(name: "CodeLinkEngineTests", dependencies: [
            "CodeLinkEngine",
            .product(name: "Hummingbird", package: "hummingbird"),
            .product(name: "ServiceLifecycle", package: "swift-service-lifecycle"),
            .product(name: "Logging", package: "swift-log"),
            .product(name: "NIOCore", package: "swift-nio"),
            .product(name: "HTTPTypes", package: "swift-http-types"),
        ]),
    ]
)
