import Foundation

/// Streaming counterpart to `SimSlimBackend`.
///
/// Every existing backend call is one-shot: run the CLI, read to EOF, decode.
/// A run cannot work that way — it lasts minutes and its whole value is watching
/// it happen — so this adds a second path that yields events as the CLI emits
/// them, line by line, and leaves the existing `execute` untouched.
struct RunBackend {
  private let executableURL: URL

  init(bundle: Bundle = .main) throws {
    if let override = ProcessInfo.processInfo.environment["SIMBERTH_CLI"], !override.isEmpty {
      executableURL = URL(fileURLWithPath: override)
    } else if let bundled = bundle.url(forResource: "simberth", withExtension: nil) {
      executableURL = bundled
    } else {
      throw BackendError.executableMissing
    }
    guard FileManager.default.isExecutableFile(atPath: executableURL.path) else {
      throw BackendError.executableMissing
    }
  }

  /// Starts a run and streams its events. The returned handle can cancel the
  /// run; letting the stream terminate also terminates the process, so closing
  /// the section never leaves an orphaned CLI holding simulators.
  func run(_ draft: ScenarioDraft) -> (
    stream: AsyncThrowingStream<RunEvent, Error>, cancel: () -> Void
  ) {
    var arguments = [
      "run", "--json",
      "--scenario", draft.text,
      "--sims", String(draft.simCount),
    ]
    if !draft.slim { arguments.append(contentsOf: ["--slim=false"]) }
    if !draft.appPath.isEmpty { arguments.append(contentsOf: ["--app", draft.appPath]) }

    return stream(arguments: arguments)
  }

  /// Runs the CLI and decodes one `RunEvent` per line of stdout.
  private func stream(arguments: [String]) -> (
    stream: AsyncThrowingStream<RunEvent, Error>, cancel: () -> Void
  ) {
    let process = Process()
    process.executableURL = executableURL
    process.arguments = arguments
    process.standardInput = FileHandle.nullDevice

    let output = Pipe()
    let errors = Pipe()
    process.standardOutput = output
    process.standardError = errors

    let stream = AsyncThrowingStream<RunEvent, Error> { continuation in
      // stdout is NDJSON; stderr is human progress, captured only so a failure
      // can be explained rather than reported as a bare exit code.
      var pending = Data()
      var diagnostics = Data()
      let decoder = JSONDecoder()

      errors.fileHandleForReading.readabilityHandler = { handle in
        diagnostics.append(handle.availableData)
      }

      output.fileHandleForReading.readabilityHandler = { handle in
        let chunk = handle.availableData
        guard !chunk.isEmpty else { return }
        pending.append(chunk)

        // A chunk boundary can split a line, so only whole lines are decoded
        // and the remainder is carried forward.
        while let newline = pending.firstIndex(of: UInt8(ascii: "\n")) {
          let line = pending[pending.startIndex..<newline]
          pending.removeSubrange(pending.startIndex...newline)
          guard !line.isEmpty else { continue }
          if let event = try? decoder.decode(RunEvent.self, from: Data(line)) {
            continuation.yield(event)
          }
        }
      }

      process.terminationHandler = { finished in
        output.fileHandleForReading.readabilityHandler = nil
        errors.fileHandleForReading.readabilityHandler = nil

        // A run that failed its scenario exits non-zero by design, and that is
        // a result rather than an error: the events already carry the verdict.
        // Only a process that produced no verdict at all is a real failure.
        if finished.terminationStatus > 1 {
          let detail = String(decoding: diagnostics, as: UTF8.self)
          continuation.finish(
            throwing: BackendError.commandFailed(
              arguments: arguments,
              exitCode: finished.terminationStatus,
              output: detail.trimmingCharacters(in: .whitespacesAndNewlines)))
        } else {
          continuation.finish()
        }
      }

      continuation.onTermination = { _ in
        if process.isRunning { process.terminate() }
      }

      do {
        try process.run()
      } catch {
        continuation.finish(throwing: error)
      }
    }

    return (stream, { if process.isRunning { process.terminate() } })
  }
}
