import * as React from "react"
import { useEffect, useRef, useState } from "react"
import { ImagePlus, Loader2, Upload, X } from "lucide-react"
import { Button } from "./button"
import { cn } from "@/lib/utils"
import { getAuthHeader } from "@/lib/auth"
import { API_BASE } from "@/lib/api"
import { toast } from "sonner"

/**
 * shadcn-style file upload primitive for reference images.
 *
 * - Controlled: parent owns `value` and is notified via `onChange`.
 * - On file pick, the component uploads the file to `POST /v1/files`
 *   and reports the returned `file_id` (so the backend can avoid
 *   receiving large base64 payloads in JSON bodies). The component
 *   also surfaces a local `previewUrl` (object URL) so the parent
 *   can fall back to it when no `file_id` is available yet.
 * - No external dependencies beyond the existing toolset.
 */

const DEFAULT_ACCEPT =
  "image/png,image/jpeg,image/jpg,image/webp,image/gif,image/bmp,image/svg+xml"

const DEFAULT_MAX_SIZE_MB = 10

export interface FileUploadValue {
  /** Backend-issued file identifier. Preferred when sending the asset downstream. */
  fileId?: string
  /** Local preview URL (object URL) or remote URL used as a fallback. */
  previewUrl?: string
  /** Original filename for display. */
  filename?: string
}

export interface FileUploadProps {
  value: FileUploadValue | null
  onChange: (next: FileUploadValue | null) => void
  /** Comma-separated list of MIME types accepted by the picker. */
  accept?: string
  /** Disable the picker (e.g. while the parent request is in flight). */
  disabled?: boolean
  /** Maximum upload size in megabytes. */
  maxSizeMB?: number
  /** Optional label shown above the picker. */
  label?: string
  /** Optional helper text shown next to / under the picker. */
  description?: string
  className?: string
}

async function readErrorDetail(res: Response): Promise<string> {
  try {
    const data = (await res.clone().json()) as {
      detail?: unknown
      error?: unknown
      message?: unknown
    }
    return String(data?.detail || data?.error || data?.message || "").trim()
  } catch {
    return ""
  }
}

export function FileUpload({
  value,
  onChange,
  accept = DEFAULT_ACCEPT,
  disabled,
  maxSizeMB = DEFAULT_MAX_SIZE_MB,
  label,
  description,
  className,
}: FileUploadProps) {
  const inputRef = useRef<HTMLInputElement | null>(null)
  const ownedObjectUrlRef = useRef<string | null>(null)
  const [uploading, setUploading] = useState(false)
  const [localError, setLocalError] = useState<string | null>(null)
  const isDisabled = disabled || uploading

  // Revoke any object URL we created when the controlled value no longer
  // references it (parent cleared the value, or replaced the preview URL).
  useEffect(() => {
    const owned = ownedObjectUrlRef.current
    if (!owned) return
    if (!value) {
      URL.revokeObjectURL(owned)
      ownedObjectUrlRef.current = null
      return
    }
    if (value.previewUrl && value.previewUrl !== owned) {
      URL.revokeObjectURL(owned)
      ownedObjectUrlRef.current = null
    }
  }, [value])

  // Final cleanup on unmount.
  useEffect(() => {
    return () => {
      if (ownedObjectUrlRef.current) {
        URL.revokeObjectURL(ownedObjectUrlRef.current)
        ownedObjectUrlRef.current = null
      }
    }
  }, [])

  const triggerPicker = () => {
    if (isDisabled) return
    inputRef.current?.click()
  }

  const handleFileChange = async (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0]
    // Reset the input so the same file can be re-selected later.
    e.target.value = ""
    if (!file) return

    if (file.size > maxSizeMB * 1024 * 1024) {
      const msg = `文件超过 ${maxSizeMB}MB 上限`
      setLocalError(msg)
      toast.error(msg)
      return
    }

    // Build a local preview before hitting the network so the UI feels instant.
    const objectUrl = URL.createObjectURL(file)
    if (ownedObjectUrlRef.current) {
      URL.revokeObjectURL(ownedObjectUrlRef.current)
    }
    ownedObjectUrlRef.current = objectUrl

    setLocalError(null)
    setUploading(true)

    try {
      const form = new FormData()
      form.append("file", file)
      const res = await fetch(`${API_BASE}/v1/files`, {
        method: "POST",
        headers: { ...getAuthHeader() },
        body: form,
      })
      if (!res.ok) {
        const detail = await readErrorDetail(res)
        throw new Error(detail || `上传失败 HTTP ${res.status}`)
      }
      const data = (await res.json()) as { id?: string; filename?: string }
      if (!data?.id) {
        throw new Error("上传成功但未返回 file_id")
      }
      onChange({
        fileId: data.id,
        previewUrl: objectUrl,
        filename: data.filename || file.name,
      })
      toast.success("参考图上传成功")
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : "上传失败"
      setLocalError(msg)
      toast.error(`参考图上传失败: ${msg.slice(0, 120)}`)
      // Roll back the optimistic preview we created.
      if (ownedObjectUrlRef.current === objectUrl) {
        URL.revokeObjectURL(objectUrl)
        ownedObjectUrlRef.current = null
      }
    } finally {
      setUploading(false)
    }
  }

  const handleRemove = () => {
    if (isDisabled) return
    if (ownedObjectUrlRef.current) {
      URL.revokeObjectURL(ownedObjectUrlRef.current)
      ownedObjectUrlRef.current = null
    }
    onChange(null)
    setLocalError(null)
  }

  const hasFile = !!(value?.fileId || value?.previewUrl)
  const displayName = value?.filename || value?.fileId || "已选择文件"

  return (
    <div className={cn("space-y-2", className)}>
      {label && <label className="text-sm font-medium">{label}</label>}
      <input
        ref={inputRef}
        type="file"
        accept={accept}
        className="hidden"
        onChange={handleFileChange}
        disabled={isDisabled}
      />
      <div
        className={cn(
          "admin-input flex items-center gap-3 p-3",
          isDisabled && "opacity-60",
        )}
      >
        {hasFile && value?.previewUrl ? (
          <img
            src={value.previewUrl}
            alt={displayName}
            className="h-14 w-14 rounded-md object-cover border border-border/60 bg-muted/30"
          />
        ) : hasFile ? (
          <div className="h-14 w-14 rounded-md border border-border/60 bg-muted/30 flex items-center justify-center text-muted-foreground">
            <ImagePlus className="h-5 w-5" />
          </div>
        ) : (
          <div className="h-14 w-14 rounded-md border border-dashed border-border/60 flex items-center justify-center text-muted-foreground">
            <ImagePlus className="h-5 w-5" />
          </div>
        )}
        <div className="min-w-0 flex-1 space-y-0.5">
          <div className="text-sm font-medium truncate">{displayName}</div>
          <div className="text-xs text-muted-foreground truncate font-mono">
            {uploading
              ? "正在上传..."
              : value?.fileId
                ? `file_id: ${value.fileId}`
                : description || `支持 PNG / JPEG / WebP / GIF，单文件 ≤ ${maxSizeMB}MB`}
          </div>
          {localError && !uploading && (
            <div className="text-xs text-red-500 mt-1">{localError}</div>
          )}
        </div>
        {hasFile ? (
          <Button
            type="button"
            variant="ghost"
            size="sm"
            onClick={handleRemove}
            disabled={isDisabled}
            className="gap-1.5"
          >
            <X className="h-4 w-4" /> 移除
          </Button>
        ) : (
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={triggerPicker}
            disabled={isDisabled}
            className="gap-1.5"
          >
            {uploading ? (
              <>
                <Loader2 className="h-4 w-4 animate-spin" /> 上传中
              </>
            ) : (
              <>
                <Upload className="h-4 w-4" /> 选择文件
              </>
            )}
          </Button>
        )}
      </div>
    </div>
  )
}

FileUpload.displayName = "FileUpload"
