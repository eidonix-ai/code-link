// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "CodeLinkEngine",
    platforms: [.macOS(.v15)],
    products: [.library(name: "CodeLinkEngine", targets: ["CodeLinkEngine"])],
    dependencies: [
        .package(url: "https://github.com/hummingbird-project/hummingbird.git", from: "2.0.0"),
    ],
    targets: [
        .target(name: "CodeLinkEngine", dependencies: [.product(name: "Hummingbird", package: "hummingbird")]),
        .testTarget(name: "CodeLinkEngineTests", dependencies: ["CodeLinkEngine"]),
    ]
)
