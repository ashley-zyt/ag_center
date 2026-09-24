# frozen_string_literal: true

# Rails(Ruby) 端调用发布服务的签名示例。
#
# 与服务端 internal/auth/auth.go 的签名规范保持一致：
#   bodyHash  = hex(sha256(原始请求体字节))
#   canonical = METHOD + "\n" + PATH + "\n" + timestamp + "\n" + nonce + "\n" + bodyHash
#   signature = hex(HMAC-SHA256(API_SECRET, canonical))
#
# 依赖 Ruby 标准库（openssl / securerandom / net/http / digest / json），无需额外 gem。
#
# 用法示例：
#   body = { profile_name: 'fb001', title: '夏日促销',
#            video_oss_url: 'https://oss.example.com/a.mp4' }.to_json
#   resp = ApiAuth.post('/facebook/publish', body)
#
# 环境变量：API_KEY（密钥）、API_SECRET（签名密钥，未设置时回退 API_KEY）。

require 'openssl'
require 'securerandom'
require 'net/http'
require 'uri'
require 'json'
require 'digest'

module ApiAuth
  BASE_URL = ENV['PUBLISH_API_BASE_URL'] || 'https://你的域名'

  HEADER_KEY       = 'X-API-Key'.freeze
  HEADER_TIMESTAMP = 'X-Timestamp'.freeze
  HEADER_NONCE     = 'X-Nonce'.freeze
  HEADER_SIGNATURE = 'X-Signature'.freeze

  module_function

  # 生成 4 个鉴权请求头。body 必须是已序列化好的字符串（与最终发送的字节完全一致）。
  def auth_headers(method:, path:, body:)
    timestamp = Time.now.to_i.to_s
    nonce = SecureRandom.hex(16)
    body_hash = Digest::SHA256.hexdigest(body)
    canonical = [method, path, timestamp, nonce, body_hash].join("\n")
    signature = OpenSSL::HMAC.hexdigest('SHA256', api_secret, canonical)

    {
      HEADER_KEY       => api_key,
      HEADER_TIMESTAMP => timestamp,
      HEADER_NONCE     => nonce,
      HEADER_SIGNATURE => signature
    }
  end

  # 发送一个 POST JSON 请求，返回 Net::HTTPResponse。
  def post(path, body_hash_or_string)
    body = body_hash_or_string.is_a?(String) ? body_hash_or_string : JSON.generate(body_hash_or_string)
    uri = URI.join(BASE_URL, path)
    http = Net::HTTP.new(uri.host, uri.port)
    http.use_ssl = uri.scheme == 'https'
    http.open_timeout = 10
    http.read_timeout = 120 # 发布流程较慢，超时放宽

    request = Net::HTTP::Post.new(uri.request_uri, headers(method: 'POST', path: path, body: body))
    request.body = body
    http.request(request)
  end

  def headers(method:, path:, body:)
    { 'Content-Type' => 'application/json' }.merge(auth_headers(method: method, path: path, body: body))
  end

  def api_key
    ENV['API_KEY'] || raise('API_KEY 环境变量未设置')
  end

  def api_secret
    ENV['API_SECRET'] || api_key
  end
end
