import AppKit
import SwiftUI

/// Owns the live run. Kept separate from `AppModel` so the simulator section is
/// untouched by anything happening here.
@MainActor
final class RunsModel: ObservableObject {
  @Published var draft = ScenarioDraft.fromEnvironment()
  @Published private(set) var run: LiveRun?
  @Published private(set) var isRunning = false
  @Published var selectedUDID: String?
  @Published var failure: PresentedError?

  private let backend: RunBackend?
  private var cancelCurrent: (() -> Void)?

  init() {
    backend = try? RunBackend()
  }

  var backendAvailable: Bool { backend != nil }

  func start() {
    guard let backend, !isRunning, draft.isRunnable else { return }

    // A new run replaces the old one; history lives on disk, not in memory.
    run = LiveRun(scenario: draft.text)
    selectedUDID = nil
    isRunning = true

    let (stream, cancel) = backend.run(draft)
    cancelCurrent = cancel

    Task { [weak self] in
      do {
        for try await event in stream {
          guard let self else { return }
          self.run?.apply(event)
          // Select the first simulator automatically so the timeline is
          // populated without the user having to click.
          if self.selectedUDID == nil, let first = self.run?.sims.first {
            self.selectedUDID = first.udid
          }
        }
      } catch {
        self?.failure = PresentedError(message: error.localizedDescription)
        self?.run?.status = .error
      }
      self?.isRunning = false
      self?.cancelCurrent = nil
    }
  }

  func stop() {
    cancelCurrent?()
    cancelCurrent = nil
    isRunning = false
  }

  /// Simulators that are booted and worth mirroring. Driven off the run's sims
  /// so mirroring starts as soon as the fleet is ready and stops when it ends.
  var readySimUDIDs: [String] {
    guard let run, !run.isFinished else { return [] }
    return run.sims.map(\.udid)
  }

  var selectedSim: SimRun? {
    guard let selectedUDID else { return run?.sims.first }
    return run?.sims.first { $0.udid == selectedUDID }
  }
}

struct RunsView: View {
  @StateObject private var model = RunsModel()
  @StateObject private var mirror = MirrorSession.shared

  var body: some View {
    HSplitView {
      VStack(spacing: 0) {
        scenarioBar
        Divider()
        fleet
      }
      .frame(minWidth: 420)

      timeline
        .frame(minWidth: 340)
    }
    .background(Color(nsColor: .windowBackgroundColor))
    .alert(item: $model.failure) { error in
      Alert(title: Text("Run failed"), message: Text(error.message))
    }
    // Mirroring follows the fleet: it starts once the simulators are booted and
    // stops when the run ends, so no capture process outlives what it shows.
    .onChange(of: model.readySimUDIDs) { _, udids in
      if udids.isEmpty {
        mirror.stop()
      } else {
        mirror.start(udids: udids)
      }
    }
    .onDisappear { mirror.stop() }
  }

  // MARK: scenario

  private var scenarioBar: some View {
    VStack(alignment: .leading, spacing: 10) {
      Text("Scenario")
        .font(.headline)

      TextEditor(text: $model.draft.text)
        .font(.body)
        .frame(height: 58)
        .padding(6)
        .background(Color(nsColor: .textBackgroundColor))
        .clipShape(RoundedRectangle(cornerRadius: 6))
        .overlay(
          RoundedRectangle(cornerRadius: 6)
            .stroke(Color(nsColor: .separatorColor))
        )
        .overlay(alignment: .topLeading) {
          if model.draft.text.isEmpty {
            Text("Sign in with test@example.com and confirm the home screen loads")
              .font(.body)
              .foregroundStyle(.tertiary)
              .padding(.horizontal, 11)
              .padding(.vertical, 14)
              .allowsHitTesting(false)
          }
        }

      HStack(spacing: 12) {
        appPicker

        Stepper(value: $model.draft.simCount, in: 1...12) {
          Text("\(model.draft.simCount) simulator\(model.draft.simCount == 1 ? "" : "s")")
            .font(.callout)
        }
        .fixedSize()

        Toggle("Slim", isOn: $model.draft.slim)
          .toggleStyle(.checkbox)
          .help("Disable unneeded daemons before booting, so more simulators fit")

        Spacer()

        if model.isRunning {
          Button("Stop", role: .destructive) { model.stop() }
        } else {
          Button("Run") { model.start() }
            .keyboardShortcut(.return, modifiers: .command)
            .disabled(!model.draft.isRunnable || !model.backendAvailable)
        }
      }
    }
    .padding(16)
  }

  private var appPicker: some View {
    HStack(spacing: 6) {
      Button {
        let panel = NSOpenPanel()
        panel.canChooseDirectories = true
        panel.canChooseFiles = true
        panel.allowedContentTypes = [.applicationBundle]
        panel.prompt = "Choose"
        if panel.runModal() == .OK, let url = panel.url {
          model.draft.appPath = url.path
        }
      } label: {
        Label("App", systemImage: "app.badge")
      }
      .help("A simulator .app bundle to install and launch (optional)")

      if !model.draft.appPath.isEmpty {
        Text((model.draft.appPath as NSString).lastPathComponent)
          .font(.callout)
          .foregroundStyle(.secondary)
          .lineLimit(1)
        Button {
          model.draft.appPath = ""
        } label: {
          Image(systemName: "xmark.circle.fill")
        }
        .buttonStyle(.plain)
        .foregroundStyle(.tertiary)
      }
    }
  }

  // MARK: fleet

  private var fleet: some View {
    Group {
      if let run = model.run, !run.sims.isEmpty {
        fleetGrid(for: run.sims)
      } else {
        emptyFleet
      }
    }
    .frame(maxWidth: .infinity, maxHeight: .infinity)
  }

  private static let fleetColumns = [GridItem(.adaptive(minimum: 200), spacing: 12)]

  // Split out of `fleet` because the inlined grid, cards and gesture together
  // exceeded what the type-checker would solve in reasonable time.
  private func fleetGrid(for sims: [SimRun]) -> some View {
    ScrollView {
      LazyVGrid(columns: Self.fleetColumns, spacing: 12) {
        ForEach(sims) { sim in
          card(for: sim)
        }
      }
      .padding(16)
    }
  }

  private func card(for sim: SimRun) -> some View {
    SimCard(
      sim: sim,
      screen: mirror.screens[sim.udid],
      isSelected: sim.udid == model.selectedUDID
    )
    .onTapGesture { model.selectedUDID = sim.udid }
  }

  private var emptyFleet: some View {
    VStack(spacing: 8) {
      Image(systemName: "play.circle")
        .font(.system(size: 34))
        .foregroundStyle(.tertiary)
      Text(model.backendAvailable ? "No run yet" : "The bundled simberth CLI is missing")
        .foregroundStyle(.secondary)
      if model.backendAvailable {
        Text("Describe what to verify, then press Run.")
          .font(.callout)
          .foregroundStyle(.tertiary)
      }
    }
    .frame(maxWidth: .infinity, maxHeight: .infinity)
  }

  // MARK: timeline

  private var timeline: some View {
    VStack(alignment: .leading, spacing: 0) {
      HStack {
        Text(model.selectedSim?.simName ?? "Steps")
          .font(.headline)
        Spacer()
        if let run = model.run {
          Label(run.status.title, systemImage: run.status.systemImage)
            .font(.callout)
            .foregroundStyle(color(for: run.status))
          Text(run.elapsedText)
            .font(.callout)
            .foregroundStyle(.secondary)
            .monospacedDigit()
        }
      }
      .padding(.horizontal, 16)
      .padding(.vertical, 12)

      Divider()

      if let sim = model.selectedSim {
        ScrollView {
          VStack(alignment: .leading, spacing: 0) {
            ForEach(sim.steps) { step in
              StepRow(step: step)
              Divider()
            }

            if let thinking = sim.thinking, !thinking.isEmpty, sim.status == .running {
              Text(thinking)
                .font(.callout)
                .foregroundStyle(.secondary)
                .padding(16)
            }

            if let detail = sim.detail, !detail.isEmpty {
              VStack(alignment: .leading, spacing: 6) {
                Label(sim.status.title, systemImage: sim.status.systemImage)
                  .font(.callout.bold())
                  .foregroundStyle(color(for: sim.status))
                Text(detail)
                  .font(.callout)
                  .foregroundStyle(.secondary)
                  .fixedSize(horizontal: false, vertical: true)
              }
              .frame(maxWidth: .infinity, alignment: .leading)
              .padding(16)
            }
          }
        }
      } else {
        Text("Select a simulator to see what it did.")
          .font(.callout)
          .foregroundStyle(.tertiary)
          .frame(maxWidth: .infinity, maxHeight: .infinity)
      }
    }
    .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .top)
  }
}

/// One simulator in the fleet: its live screen, who it is, and what it is doing.
/// The screen is the point — a run is far easier to follow by watching the
/// phones than by reading a step list.
private struct SimCard: View {
  let sim: SimRun
  let screen: NSImage?
  let isSelected: Bool

  var body: some View {
    VStack(alignment: .leading, spacing: 8) {
      screenView

      HStack(spacing: 6) {
        Image(systemName: sim.status.systemImage)
          .foregroundStyle(color(for: sim.status))
        Text(sim.simName)
          .font(.callout.bold())
          .lineLimit(1)
        Spacer()
        if sim.status == .running {
          ProgressView().controlSize(.small)
        }
      }

      Text(sim.currentStep?.summary ?? "waiting…")
        .font(.caption)
        .foregroundStyle(.secondary)
        .lineLimit(2)
        .frame(maxWidth: .infinity, alignment: .leading)
        .frame(height: 28, alignment: .top)

      Text("\(sim.passedSteps)/\(sim.steps.count) steps")
        .font(.caption)
        .foregroundStyle(.tertiary)
        .monospacedDigit()
    }
    .padding(12)
    .frame(maxWidth: .infinity, alignment: .leading)
    .background(Color(nsColor: .controlBackgroundColor))
    .clipShape(RoundedRectangle(cornerRadius: 10))
    .overlay(
      RoundedRectangle(cornerRadius: 10)
        .stroke(
          isSelected ? Color.accentColor : Color(nsColor: .separatorColor),
          lineWidth: isSelected ? 2 : 1)
    )
    .contentShape(Rectangle())
  }

  /// The live screen, in a phone-shaped frame. Until the first frame arrives
  /// the placeholder holds the same aspect ratio, so the grid never reflows.
  private var screenView: some View {
    ZStack {
      RoundedRectangle(cornerRadius: 12)
        .fill(Color.black)

      if let screen {
        Image(nsImage: screen)
          .resizable()
          .interpolation(.medium)
          .aspectRatio(contentMode: .fit)
          .clipShape(RoundedRectangle(cornerRadius: 12))
          .transition(.opacity)
      } else {
        VStack(spacing: 6) {
          Image(systemName: "iphone")
            .font(.system(size: 22))
            .foregroundStyle(.tertiary)
          Text(sim.status == .running ? "connecting…" : "no signal")
            .font(.caption2)
            .foregroundStyle(.tertiary)
        }
      }
    }
    .aspectRatio(0.46, contentMode: .fit)
    .frame(maxWidth: .infinity)
    .overlay(
      RoundedRectangle(cornerRadius: 12)
        .stroke(Color(nsColor: .separatorColor), lineWidth: 1)
    )
  }
}

private struct StepRow: View {
  let step: RunStep

  var body: some View {
    HStack(alignment: .top, spacing: 10) {
      Image(systemName: step.status.systemImage)
        .foregroundStyle(color(for: step.status))
        .frame(width: 16)

      VStack(alignment: .leading, spacing: 2) {
        Text(step.summary.isEmpty ? step.tool : step.summary)
          .font(.callout)
          .fixedSize(horizontal: false, vertical: true)
        if let detail = step.detail, !detail.isEmpty {
          Text(detail)
            .font(.caption)
            .foregroundStyle(.secondary)
            .fixedSize(horizontal: false, vertical: true)
        }
      }

      Spacer()

      if step.status != .running {
        Text(step.durationText)
          .font(.caption)
          .foregroundStyle(.tertiary)
          .monospacedDigit()
      }
    }
    .padding(.horizontal, 16)
    .padding(.vertical, 8)
  }
}

private func color(for status: RunStatus) -> Color {
  switch status {
  case .passed: return .green
  case .failed: return .red
  case .error: return .orange
  case .running: return .secondary
  case .skipped: return .secondary
  }
}
