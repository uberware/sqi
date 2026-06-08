import { Component } from 'react'
import type { ErrorInfo, ReactNode } from 'react'
import styles from './ErrorBoundary.module.css'

interface Props {
  children?: ReactNode
  fallbackTitle?: string
}

interface State {
  hasError: boolean
  errorMessage: string | null
}

export default class ErrorBoundary extends Component<Props, State> {
  constructor(props: Props) {
    super(props)
    this.state = { hasError: false, errorMessage: null }
  }

  static getDerivedStateFromError(error: unknown): State {
    const errorMessage = error instanceof Error ? error.message : String(error)
    return { hasError: true, errorMessage }
  }

  componentDidCatch(error: unknown, info: ErrorInfo): void {
    console.error('[ErrorBoundary]', error, info.componentStack)
  }

  private reset = (): void => {
    this.setState({ hasError: false, errorMessage: null })
  }

  render(): ReactNode {
    if (this.state.hasError) {
      return (
        <div className={styles.errorCard} role="alert">
          <span className={styles.icon} aria-hidden="true">
            ⚠
          </span>
          <div className={styles.content}>
            <h2 className={styles.title}>{this.props.fallbackTitle ?? 'Something went wrong'}</h2>
            {this.state.errorMessage !== null && (
              <p className={styles.message}>{this.state.errorMessage}</p>
            )}
            <button className={styles.retryButton} onClick={this.reset} type="button">
              Reload this section
            </button>
          </div>
        </div>
      )
    }

    return this.props.children ?? null
  }
}
